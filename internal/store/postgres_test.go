package store

import (
	"context"
	"errors"
	"fmt"
	"retro/internal/model"
	"retro/internal/testutil"
	"strings"
	"sync"
	"testing"
	"time"
)

func postgresPair(t *testing.T) (*PostgresStore, *PostgresStore) {
	t.Helper()
	dsn := testutil.PostgresURL(t)
	a, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	if err = a.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = a.Migrate(context.Background()); err != nil {
		t.Fatal("повтор миграции: ", err)
	}
	b, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return a, b
}

func TestPostgresConcurrentUpdatesAndRollback(t *testing.T) {
	a, b := postgresPair(t)
	board, _ := model.NewBoard("Postgres", "ssc", "owner-1234")
	if err := a.Create(board); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st := a
			if i%2 == 0 {
				st = b
			}
			_, err := st.Update(board.ID, func(x *model.Board) error {
				_, err := x.AddCard(x.Columns[0].ID, fmt.Sprint(i), "", fmt.Sprintf("author-%04d", i))
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	got, err := b.Get(board.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Columns[0].Cards) != 30 || got.Revision != board.Revision+30 {
		t.Fatalf("потеря обновления: cards=%d revision=%d", len(got.Columns[0].Cards), got.Revision)
	}
	revision := got.Revision
	_, err = a.Update(board.ID, func(x *model.Board) error { x.Title = "не сохранять"; return model.ErrConflict })
	if !errors.Is(err, model.ErrConflict) {
		t.Fatal(err)
	}
	got, _ = b.Get(board.ID)
	if got.Title != board.Title || got.Revision != revision {
		t.Fatal("ошибка мутации изменила данные")
	}
	got.Title = "копия"
	got, _ = b.Get(board.ID)
	if got.Title != board.Title {
		t.Fatal("Get вернул разделяемый объект")
	}
}

func TestPostgresImportAndNextAreAtomic(t *testing.T) {
	a, b := postgresPair(t)
	prev, _ := model.NewBoard("Предыдущее", "ssc", "owner-1234")
	prev.Phase = model.PhaseDiscuss
	if err := a.Create(prev); err != nil {
		t.Fatal(err)
	}
	added, _ := model.NewBoard("Импорт", "ssc", "owner-1234")
	if err := a.CreateMany([]*model.Board{added, prev}); err == nil {
		t.Fatal("дубликат принят")
	}
	if _, err := b.Get(added.ID); !errors.Is(err, model.ErrNotFound) {
		t.Fatal("частичный импорт сохранился")
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, st := range []*PostgresStore{a, b} {
		wg.Add(1)
		go func(st *PostgresStore) {
			defer wg.Done()
			_, err := st.CreateNext(prev.ID, "Следующее", prev.OwnerID)
			results <- err
		}(st)
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("создано следующих ретро: %d", success)
	}
	saved, _ := a.Get(prev.ID)
	next, err := b.Get(saved.NextID)
	if err != nil || next.PrevID != prev.ID || saved.Revision != prev.Revision+1 {
		t.Fatal("ссылки/ревизия не сохранены атомарно")
	}
}

func TestPostgresCapacityAcrossInstances(t *testing.T) {
	a, b := postgresPair(t)
	boards := make([]*model.Board, MaxBoards-1)
	for i := range boards {
		boards[i], _ = model.NewBoard(fmt.Sprint(i), "ssc", "owner-1234")
	}
	if err := a.CreateMany(boards); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 2)
	for _, st := range []*PostgresStore{a, b} {
		go func(st *PostgresStore) {
			board, _ := model.NewBoard("Последнее", "ssc", "owner-1234")
			errs <- st.Create(board)
		}(st)
	}
	success, capacity := 0, 0
	for i := 0; i < 2; i++ {
		err := <-errs
		if err == nil {
			success++
		} else if errors.Is(err, model.ErrCapacity) {
			capacity++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || capacity != 1 {
		t.Fatalf("квота: %d %d", success, capacity)
	}
}

func TestPostgresTimersReadinessAndSafeErrors(t *testing.T) {
	a, b := postgresPair(t)
	board, _ := model.NewBoard("Таймер", "ssc", "owner-1234")
	past := time.Now().Add(-time.Minute)
	board.TimerEndsAt = &past
	if err := a.Create(board); err != nil {
		t.Fatal(err)
	}
	ids, err := b.DueTimers(context.Background(), time.Now())
	if err != nil || len(ids) != 1 || ids[0] != board.ID {
		t.Fatalf("таймер: %v %v", ids, err)
	}
	if _, err = a.Update(board.ID, func(x *model.Board) error { x.TimerEndsAt = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	ids, err = b.DueTimers(context.Background(), time.Now())
	if err != nil || len(ids) != 0 {
		t.Fatal("снятый таймер остался в индексе")
	}
	if err := a.Ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Ready(context.Background()); err == nil {
		t.Fatal("закрытая БД прошла readiness")
	}
	_, err = OpenPostgres("postgres://user:SECRET_PASSWORD@[broken")
	if err == nil || strings.Contains(err.Error(), "SECRET_PASSWORD") {
		t.Fatalf("секрет в ошибке: %v", err)
	}
}
