package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"retro/internal/model"
)

func atomicTestBoard(t *testing.T, title string) *model.Board {
	t.Helper()
	b, err := model.NewBoard(title, "classic", "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Ошибка вставки в SQLite возникает после UPDATE предыдущей доски или
// после первой вставки импорта. В JSON ошибка записи проверяет откат памяти.
func failBoardWrite(t *testing.T, st Store, title string) func() {
	t.Helper()
	switch s := st.(type) {
	case *SQLiteStore:
		// title выбирается только из констант теста.
		_, err := s.db.Exec(`CREATE TRIGGER fail_board_write BEFORE INSERT ON boards
			WHEN json_extract(NEW.data, '$.title') = '` + title + `'
			BEGIN SELECT RAISE(ABORT, 'ошибка записи для теста'); END`)
		if err != nil {
			t.Fatal(err)
		}
		return func() {
			if _, err := s.db.Exec(`DROP TRIGGER fail_board_write`); err != nil {
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

func assertBoardCount(t *testing.T, st Store, want int) {
	t.Helper()
	var got int
	switch s := st.(type) {
	case *SQLiteStore:
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM boards`).Scan(&got); err != nil {
			t.Fatal(err)
		}
	case *JSONStore:
		s.mu.RLock()
		got = len(s.boards)
		s.mu.RUnlock()
	}
	if got != want {
		t.Fatalf("число досок: ожидалось %d, получено %d", want, got)
	}
}

func TestCreateNextConcurrentAndPersistent(t *testing.T) {
	for _, file := range []string{"boards.json", "retro.db"} {
		t.Run(file, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), file)
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			prev := atomicTestBoard(t, "Спринт 1")
			prev.Phase = model.PhaseDiscuss
			if _, err := prev.AddAction("Открытое решение", "Анна", "alice"); err != nil {
				t.Fatal(err)
			}
			if err := st.Create(prev); err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateNext(prev.ID, "Нет прав", "alice"); !errors.Is(err, model.ErrForbidden) {
				t.Fatalf("чужой участник не должен создавать следующее ретро: %v", err)
			}
			if _, err := st.CreateNext("missing", "Нет доски", "owner-1"); !errors.Is(err, model.ErrNotFound) {
				t.Fatalf("несуществующая предыдущая доска: %v", err)
			}

			type result struct {
				board *model.Board
				err   error
			}
			results := make(chan result, 12)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for range cap(results) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					b, err := st.CreateNext(prev.ID, "Спринт 2", "owner-1")
					results <- result{b, err}
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			var next *model.Board
			for r := range results {
				if r.err != nil {
					if !errors.Is(r.err, model.ErrInvalid) {
						t.Fatalf("ожидался запрет второго следующего ретро: %v", r.err)
					}
					continue
				}
				if next != nil {
					t.Fatal("конкурентные запросы создали несколько следующих ретро")
				}
				next = r.board
			}
			if next == nil {
				t.Fatal("ни одно следующее ретро не создано")
			}
			if next.PrevID != prev.ID || len(next.Actions) != 1 || next.Actions[0].CarriedFrom != prev.ID {
				t.Fatalf("связь или перенос решения потеряны: %+v", next)
			}
			// Возвращённая доска не должна быть общей с состоянием JSONStore.
			next.Title = "Внешнее изменение"
			unchanged, err := st.Get(next.ID)
			if err != nil || unchanged.Title != "Спринт 2" {
				t.Fatalf("возвращённая доска изменила хранилище: board=%+v err=%v", unchanged, err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			storedPrev, err := st.Get(prev.ID)
			if err != nil {
				t.Fatal(err)
			}
			if storedPrev.Revision != prev.Revision+1 {
				t.Fatalf("создание следующего ретро должно один раз изменить версию предыдущего: %d", storedPrev.Revision)
			}
			storedNext, err := st.Get(storedPrev.NextID)
			if err != nil {
				t.Fatalf("ссылка ведёт на отсутствующую доску: %v", err)
			}
			if storedNext.ID != next.ID || storedNext.PrevID != prev.ID || storedNext.Title != "Спринт 2" {
				t.Fatalf("доски после повторного открытия не согласованы: %+v", storedNext)
			}
			assertBoardCount(t, st, 2)
		})
	}
}

func TestCreateNextWriteFailureRollsBack(t *testing.T) {
	for _, file := range []string{"boards.json", "retro.db"} {
		t.Run(file, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), file)
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			prev := atomicTestBoard(t, "Предыдущее")
			if err := st.Create(prev); err != nil {
				t.Fatal(err)
			}
			clearFailure := failBoardWrite(t, st, "Следующее")
			if _, err := st.CreateNext(prev.ID, "Следующее", "owner-1"); err == nil {
				t.Fatal("ожидалась ошибка сохранения")
			}
			current, err := st.Get(prev.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.NextID != "" || current.Revision != prev.Revision || !current.UpdatedAt.Equal(prev.UpdatedAt) {
				t.Fatalf("неудачное создание изменило предыдущую доску: %+v", current)
			}
			assertBoardCount(t, st, 1)
			clearFailure()
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			assertBoardCount(t, st, 1)
			if _, err := st.CreateNext(prev.ID, "Следующее", "owner-1"); err != nil {
				t.Fatalf("после перезапуска создание должно быть доступно: %v", err)
			}
			assertBoardCount(t, st, 2)
		})
	}
}

func TestImportJSONWriteFailureIsAtomic(t *testing.T) {
	for _, file := range []string{"boards.json", "retro.db"} {
		t.Run(file, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, file)
			st, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			boards := []*model.Board{atomicTestBoard(t, "Первая"), atomicTestBoard(t, "Вторая")}
			data, err := json.Marshal(boards)
			if err != nil {
				t.Fatal(err)
			}
			legacy := filepath.Join(dir, "legacy.json")
			if err := os.WriteFile(legacy, data, 0o644); err != nil {
				t.Fatal(err)
			}
			clearFailure := failBoardWrite(t, st, "Вторая")
			if n, err := ImportJSON(st, legacy); err == nil || n != 0 {
				t.Fatalf("ошибка импорта должна откатить все доски: n=%d err=%v", n, err)
			}
			assertBoardCount(t, st, 0)
			clearFailure()
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			st, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			assertBoardCount(t, st, 0)
			if n, err := ImportJSON(st, legacy); err != nil || n != len(boards) {
				t.Fatalf("повторный импорт после открытия должен пройти целиком: n=%d err=%v", n, err)
			}
			assertBoardCount(t, st, len(boards))
		})
	}
}

func TestCreateManyRejectsDuplicateWithoutPartialWrites(t *testing.T) {
	for _, file := range []string{"boards.json", "retro.db"} {
		t.Run(file, func(t *testing.T) {
			st, err := Open(filepath.Join(t.TempDir(), file))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			b := atomicTestBoard(t, "Дубликат")
			if err := st.CreateMany([]*model.Board{b, b}); err == nil {
				t.Fatal("повторный id внутри набора должен быть отклонён")
			}
			assertBoardCount(t, st, 0)
		})
	}
}
