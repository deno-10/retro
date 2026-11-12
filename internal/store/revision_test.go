package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"retro/internal/model"
)

func failBoardUpdate(t *testing.T, st Store) func() {
	t.Helper()
	switch s := st.(type) {
	case *SQLiteStore:
		if _, err := s.db.Exec(`CREATE TRIGGER fail_board_update BEFORE UPDATE ON boards
			BEGIN SELECT RAISE(ABORT, 'ошибка обновления для теста'); END`); err != nil {
			t.Fatal(err)
		}
		return func() {
			if _, err := s.db.Exec(`DROP TRIGGER fail_board_update`); err != nil {
				t.Fatal(err)
			}
		}
	case *JSONStore:
		if err := os.Mkdir(s.path+".tmp", 0o755); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := os.Remove(s.path + ".tmp"); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatalf("неизвестное хранилище %T", st)
		return nil
	}
}

func TestBoardRevisionsSurviveRollbackConcurrencyAndReopen(t *testing.T) {
	for _, file := range []string{"boards.json", "retro.db"} {
		t.Run(file, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), file)
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			b := atomicTestBoard(t, "Версии")
			if err := st.Create(b); err != nil {
				t.Fatal(err)
			}
			updated, err := st.Update(b.ID, func(b *model.Board) error {
				b.Revision = 999 // Номер назначает хранилище, а не вызывающий код.
				b.Receipts = map[string]model.MutationReceipt{"saved": {Operation: "add-card", Fingerprint: "hash"}}
				return nil
			})
			if err != nil || updated.Revision != 2 {
				t.Fatalf("первая мутация должна дать версию 2: board=%+v err=%v", updated, err)
			}
			if _, err := st.Update(b.ID, func(b *model.Board) error {
				b.Revision = 1000
				b.Receipts["rejected"] = model.MutationReceipt{Operation: "add-card"}
				return model.ErrConflict
			}); !errors.Is(err, model.ErrConflict) {
				t.Fatalf("ожидался откат конфликтной мутации: %v", err)
			}
			clearFailure := failBoardUpdate(t, st)
			if _, err := st.Update(b.ID, func(b *model.Board) error {
				b.Receipts["failed"] = model.MutationReceipt{Operation: "add-action"}
				return nil
			}); err == nil {
				t.Fatal("ожидалась ошибка сохранения")
			}
			clearFailure()
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			current, err := st.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Revision != 2 || len(current.Receipts) != 1 || current.Receipts["saved"].Fingerprint != "hash" {
				t.Fatalf("неудачные мутации сохранили версию или квитанции после открытия: %+v", current)
			}

			const count = 12
			start := make(chan struct{})
			versions := make(chan uint64, count)
			errs := make(chan error, count)
			var wg sync.WaitGroup
			for i := range count {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					updated, err := st.Update(b.ID, func(b *model.Board) error {
						b.Revision = 999
						b.Receipts[fmt.Sprintf("request-%d", i)] = model.MutationReceipt{Operation: "add-card"}
						return nil
					})
					if err != nil {
						errs <- err
						return
					}
					versions <- updated.Revision
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			close(versions)
			for err := range errs {
				t.Fatal(err)
			}
			seen := map[uint64]bool{}
			for revision := range versions {
				if seen[revision] || revision < 3 || revision > 2+count {
					t.Fatalf("непоследовательная или повторная версия: %d", revision)
				}
				seen[revision] = true
			}
			if len(seen) != count {
				t.Fatalf("ожидалось %d последовательных версий, получено %d", count, len(seen))
			}
			next, err := st.CreateNext(b.ID, "Следующее", "owner-1")
			if err != nil {
				t.Fatal(err)
			}
			if next.Revision != 1 {
				t.Fatalf("новая доска должна начинаться с версии 1: %d", next.Revision)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			current, err = st.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Revision != 3+count || current.NextID != next.ID || len(current.Receipts) != 1+count {
				t.Fatalf("связь, квитанции или версия доски потеряны при открытии: %+v", current)
			}
		})
	}
}

func TestImportPreservesLegacyZeroAndExistingRevisions(t *testing.T) {
	for _, file := range []string{"boards.json", "retro.db"} {
		t.Run(file, func(t *testing.T) {
			dir := t.TempDir()
			legacy := filepath.Join(dir, "legacy.json")
			if err := os.WriteFile(legacy, []byte(`[
				{"id":"old", "title":"Старая", "template":"classic", "ownerId":"owner-1", "phase":"write", "columns":[]},
				{"id":"versioned", "revision":7, "title":"С версией", "template":"classic", "ownerId":"owner-1", "phase":"write", "columns":[]}
			]`), 0o644); err != nil {
				t.Fatal(err)
			}
			st, err := Open(filepath.Join(dir, file))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if n, err := ImportJSON(st, legacy); err != nil || n != 2 {
				t.Fatalf("импорт прежних записей: n=%d err=%v", n, err)
			}
			for id, revision := range map[string]uint64{"old": 0, "versioned": 7} {
				b, err := st.Get(id)
				if err != nil || b.Revision != revision {
					t.Fatalf("импорт изменил версию %s: board=%+v err=%v", id, b, err)
				}
				updated, err := st.Update(id, func(b *model.Board) error { b.Title = "Обновлённая"; return nil })
				if err != nil || updated.Revision != revision+1 {
					t.Fatalf("мутация должна продолжать исходную версию: board=%+v err=%v", updated, err)
				}
			}
		})
	}
}
