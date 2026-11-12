package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"retro/internal/model"
)

func TestBoardCapacityIsAtomic(t *testing.T) {
	for _, extension := range []string{"db", "json"} {
		t.Run(extension, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "boards."+extension))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			boards := make([]*model.Board, MaxBoards-1)
			for i := range boards {
				boards[i], _ = model.NewBoard(fmt.Sprint(i), "classic", "owner-00000001")
			}
			if err := s.CreateMany(boards); err != nil {
				t.Fatal(err)
			}
			one, _ := model.NewBoard("Один", "classic", "owner-00000001")
			two, _ := model.NewBoard("Два", "classic", "owner-00000001")
			if err := s.CreateMany([]*model.Board{one, two}); !errors.Is(err, model.ErrCapacity) {
				t.Fatalf("импорт за пределом: %v", err)
			}
			if _, err := s.Get(one.ID); !errors.Is(err, model.ErrNotFound) {
				t.Fatal("часть импорта сохранилась")
			}
			var success atomic.Int32
			var wg sync.WaitGroup
			for _, b := range []*model.Board{one, two} {
				wg.Go(func() {
					if err := s.Create(b); err == nil {
						success.Add(1)
					} else if !errors.Is(err, model.ErrCapacity) {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if success.Load() != 1 {
				t.Fatalf("два запроса прошли один свободный слот: %d", success.Load())
			}
			if _, err := s.CreateNext(boards[0].ID, "Следующее", boards[0].OwnerID); !errors.Is(err, model.ErrCapacity) {
				t.Fatalf("следующее ретро за пределом: %v", err)
			}
			got, _ := s.Get(boards[0].ID)
			if got.NextID != "" || got.Revision != 1 {
				t.Fatal("отказ изменил предыдущее ретро")
			}
		})
	}
}

func TestOversizedMutationRollsBack(t *testing.T) {
	for _, extension := range []string{"db", "json"} {
		s, err := Open(filepath.Join(t.TempDir(), "boards."+extension))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := model.NewBoard("Доска", "classic", "owner-00000001")
		s.Create(b)
		_, err = s.Update(b.ID, func(b *model.Board) error { b.Title = strings.Repeat("x", MaxBoardBytes); return nil })
		if !errors.Is(err, model.ErrCapacity) {
			t.Fatalf("%s: %v", extension, err)
		}
		got, _ := s.Get(b.ID)
		if got.Title != b.Title || got.Revision != b.Revision {
			t.Fatal("слишком большая мутация сохранилась")
		}
		s.Close()
	}
}
