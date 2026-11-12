package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"retro/internal/hub"
	"retro/internal/model"
	"retro/internal/store"
)

func newTimerRegressionServer(t *testing.T) (*Server, *model.Board) {
	t.Helper()
	st, err := store.OpenJSON(filepath.Join(t.TempDir(), "boards.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := model.NewBoard("Таймер", "classic", "owner-00000001")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Create(b); err != nil {
		t.Fatal(err)
	}
	s := &Server{
		store: st, hub: hub.New(), log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		timers: map[string]*phaseTimer{},
	}
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for id := range s.timers {
			s.cancelTimerLocked(id)
		}
		st.Close()
	})
	return s, b
}

func timerSnapshot(t *testing.T, s *Server, id string, endsAt *time.Time) *model.Board {
	t.Helper()
	b, err := s.store.Update(id, func(b *model.Board) error {
		b.TimerEndsAt = endsAt
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertTimerPhase(t *testing.T, s *Server, id string, phase model.Phase) {
	t.Helper()
	b, err := s.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if b.Phase != phase {
		t.Fatalf("фаза = %s, ожидается %s; конец таймера = %v", b.Phase, phase, b.TimerEndsAt)
	}
	if phase == model.PhaseReveal && b.TimerEndsAt != nil {
		t.Fatal("сработавший таймер должен быть снят")
	}
}

type pausedTimerReadStore struct {
	store.Store
	paused   atomic.Bool
	captured chan struct{}
	resume   chan struct{}
}

func (s *pausedTimerReadStore) Get(id string) (*model.Board, error) {
	b, err := s.Store.Get(id)
	if s.paused.CompareAndSwap(false, true) {
		close(s.captured)
		<-s.resume
	}
	return b, err
}

func TestTimerGETSnapshotBeforePOSTDoesNotCancelTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, b := newTimerRegressionServer(t)
		paused := &pausedTimerReadStore{Store: s.store, captured: make(chan struct{}), resume: make(chan struct{})}
		s.store = paused
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/boards/{id}", s.handleGetBoard)
		mux.HandleFunc("POST /api/boards/{id}/timer", s.handleSetTimer)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			rec, _ := do(t, mux, "GET", "/api/boards/"+b.ID, "guest-00000001", nil)
			if rec.Code != http.StatusOK {
				t.Errorf("чтение доски: %d", rec.Code)
			}
		}()
		<-paused.captured // GET уже прочитал доску без таймера.
		rec, _ := do(t, mux, "POST", "/api/boards/"+b.ID+"/timer", b.OwnerID, map[string]any{"minutes": 1})
		close(paused.resume) // POST сохранил и запланировал таймер раньше GET.
		<-finished
		if rec.Code != http.StatusOK {
			t.Fatalf("запуск таймера: %d %s", rec.Code, rec.Body.String())
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		assertTimerPhase(t, s, b.ID, model.PhaseReveal)
	})
}

func TestTimerStaleSnapshotKeepsLatestDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial time.Duration
		latest  time.Duration
	}{
		{"продление", time.Minute, 2 * time.Minute},
		{"сокращение", 2 * time.Minute, time.Minute},
		{"отмена", time.Minute, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, b := newTimerRegressionServer(t)
				initial := time.Now().Add(tc.initial)
				stale := timerSnapshot(t, s, b.ID, &initial)
				s.syncTimer(stale)
				var latest *time.Time
				if tc.latest != 0 {
					at := time.Now().Add(tc.latest)
					latest = &at
				}
				s.syncTimer(timerSnapshot(t, s, b.ID, latest))
				s.syncTimer(stale) // Старый запрос добрался до планировщика последним.
				if latest == nil {
					time.Sleep(3 * time.Minute)
					synctest.Wait()
					assertTimerPhase(t, s, b.ID, model.PhaseWrite)
					return
				}
				time.Sleep(tc.latest - time.Second)
				synctest.Wait()
				assertTimerPhase(t, s, b.ID, model.PhaseWrite)
				time.Sleep(time.Second)
				synctest.Wait()
				assertTimerPhase(t, s, b.ID, model.PhaseReveal)
			})
		})
	}
}

func TestTimerStaleCallbackCannotChangeReplacement(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "продление"
		if cancel {
			name = "отмена"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, b := newTimerRegressionServer(t)
				at := time.Now().Add(time.Minute)
				s.syncTimer(timerSnapshot(t, s, b.ID, &at))
				s.mu.Lock()
				old := s.timers[b.ID]
				s.mu.Unlock()
				var next *time.Time
				if !cancel {
					later := time.Now().Add(2 * time.Minute)
					next = &later
				}
				s.syncTimer(timerSnapshot(t, s, b.ID, next))
				// Имитируем callback, который начал исполняться до вызова Stop,
				// но дождался mu только после переноса/отмены таймера.
				s.fireTimer(b.ID, old)
				time.Sleep(time.Minute)
				synctest.Wait()
				assertTimerPhase(t, s, b.ID, model.PhaseWrite)
				time.Sleep(time.Minute)
				synctest.Wait()
				want := model.PhaseReveal
				if cancel {
					want = model.PhaseWrite
				}
				assertTimerPhase(t, s, b.ID, want)
			})
		})
	}
}

type changedTimerBeforeUpdateStore struct {
	store.Store
	changed atomic.Bool
	next    *time.Time
}

func (s *changedTimerBeforeUpdateStore) Update(id string, fn func(*model.Board) error) (*model.Board, error) {
	if s.changed.CompareAndSwap(false, true) {
		// Другой запрос записал новый дедлайн после Get, до атомарного Update.
		if _, err := s.Store.Update(id, func(b *model.Board) error {
			b.TimerEndsAt = s.next
			return nil
		}); err != nil {
			return nil, err
		}
	}
	return s.Store.Update(id, fn)
}

func TestTimerDeadlineChangedBetweenReadAndExpiry(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "продление"
		if cancel {
			name = "отмена"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, b := newTimerRegressionServer(t)
				expired := time.Now().Add(-time.Minute)
				stale := timerSnapshot(t, s, b.ID, &expired)
				changed := &changedTimerBeforeUpdateStore{Store: s.store}
				if !cancel {
					next := time.Now().Add(time.Minute)
					changed.next = &next
				}
				s.store = changed
				s.syncTimer(stale)
				assertTimerPhase(t, s, b.ID, model.PhaseWrite)
				time.Sleep(time.Minute)
				synctest.Wait()
				want := model.PhaseReveal
				if cancel {
					want = model.PhaseWrite
				}
				assertTimerPhase(t, s, b.ID, want)
			})
		})
	}
}

type retryTimerStore struct {
	store.Store
	fail atomic.Bool
}

func (s *retryTimerStore) Update(id string, fn func(*model.Board) error) (*model.Board, error) {
	if s.fail.Swap(false) {
		return nil, errors.New("временная ошибка записи")
	}
	return s.Store.Update(id, fn)
}

func TestTimerRetriesAfterStorageFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, b := newTimerRegressionServer(t)
		retry := &retryTimerStore{Store: s.store}
		s.store = retry
		at := time.Now().Add(time.Minute)
		s.syncTimer(timerSnapshot(t, s, b.ID, &at))
		retry.fail.Store(true)
		time.Sleep(time.Minute)
		synctest.Wait()
		assertTimerPhase(t, s, b.ID, model.PhaseWrite)
		time.Sleep(time.Second)
		synctest.Wait()
		assertTimerPhase(t, s, b.ID, model.PhaseReveal)
	})
}

type retryTimerReadStore struct {
	store.Store
	fail atomic.Bool
}

func (s *retryTimerReadStore) Get(id string) (*model.Board, error) {
	if s.fail.Swap(false) {
		return nil, errors.New("временная ошибка чтения")
	}
	return s.Store.Get(id)
}

func TestTimerPOSTRecoversFromFirstSyncReadFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, b := newTimerRegressionServer(t)
		retry := &retryTimerReadStore{Store: s.store}
		s.store = retry
		mux := http.NewServeMux()
		mux.HandleFunc("POST /api/boards/{id}/timer", s.handleSetTimer)
		retry.fail.Store(true)
		rec, _ := do(t, mux, "POST", "/api/boards/"+b.ID+"/timer", b.OwnerID, map[string]any{"minutes": 1})
		if rec.Code != http.StatusOK {
			t.Fatalf("запуск таймера: %d %s", rec.Code, rec.Body.String())
		}
		// POST сохранил таймер, но перечитать доску для планирования не смог.
		// Дальнейших HTTP-запросов нет; повторная попытка должна сработать сама.
		time.Sleep(time.Minute)
		synctest.Wait()
		assertTimerPhase(t, s, b.ID, model.PhaseReveal)
	})
}

func TestTimerReadFailureWithStaleEmptySnapshotRecovers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, stale := newTimerRegressionServer(t)
		retry := &retryTimerReadStore{Store: s.store}
		s.store = retry
		at := time.Now().Add(time.Minute)
		timerSnapshot(t, s, stale.ID, &at)
		retry.fail.Store(true)
		s.syncTimer(stale)
		time.Sleep(time.Minute)
		synctest.Wait()
		assertTimerPhase(t, s, stale.ID, model.PhaseReveal)
	})
}

func TestTimerReadFailureKeepsGenerationAndRecoversShorterDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, b := newTimerRegressionServer(t)
		retry := &retryTimerReadStore{Store: s.store}
		s.store = retry
		initial := time.Now().Add(2 * time.Minute)
		s.syncTimer(timerSnapshot(t, s, b.ID, &initial))
		s.mu.Lock()
		original := s.timers[b.ID]
		s.mu.Unlock()
		shorter := time.Now().Add(time.Minute)
		current := timerSnapshot(t, s, b.ID, &shorter)
		retry.fail.Store(true)
		s.syncTimer(current)
		s.mu.Lock()
		preserved := s.timers[b.ID] == original
		s.mu.Unlock()
		if !preserved {
			t.Fatal("ошибка чтения не должна заменять действующее поколение таймера")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		assertTimerPhase(t, s, b.ID, model.PhaseReveal)
	})
}
