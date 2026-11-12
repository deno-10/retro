package server

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"retro/internal/model"
	"retro/internal/store"
)

func requireExpiryNotification(t *testing.T, signals <-chan struct{}) {
	t.Helper()
	select {
	case <-signals:
	default:
		t.Fatal("сохранённое истечение таймера должно оповестить подписчиков")
	}
}

func TestExpiredTimerRejectsOldPhaseMutationAfterRestart(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		for _, kind := range []string{"card", "vote"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				card := addVoteCard(t, f, "Существующая карточка")
				path := "/api/boards/" + f.id + "/cards"
				body := f.payload("cards", "Опоздавшая карточка")
				wantPhase := model.PhaseVote
				if kind == "vote" {
					f.phase("vote")
					path += "/" + card + "/vote"
					body = map[string]any{"voted": true}
					wantPhase = model.PhaseDiscuss
				}
				setTimerEnd(t, f.store, f.id, time.Now().Add(-time.Minute))
				before := f.state()
				f.reopen()
				signals, unsubscribe := f.hub.Subscribe(f.id)
				defer unsubscribe()
				rec, _ := doMutation(t, f.handler, "POST", path, mutationAuthor, "late-request-01", body)
				requireMutationStatus(t, rec, http.StatusConflict)
				after := f.state()
				if after.Phase != wantPhase || after.TimerEndsAt != nil || after.Revision != before.Revision+1 {
					t.Fatalf("отказ не сохранил однократное истечение: %+v", after)
				}
				if len(after.Columns[0].Cards) != 1 || after.VotesUsed(mutationAuthor) != 0 || len(after.Receipts) != 0 {
					t.Fatal("запрос истёкшей фазы изменил карточки, голоса или квитанции")
				}
				requireExpiryNotification(t, signals)
				rec, _ = doMutation(t, f.handler, "POST", path, mutationAuthor, "late-request-01", body)
				requireMutationStatus(t, rec, http.StatusConflict)
				if again := f.state(); again.Revision != after.Revision || again.Phase != wantPhase {
					t.Fatal("повтор отказавшего запроса ещё раз переключил фазу или изменил версию")
				}
			})
		}
	}
}

func TestExpiredTimerAllowsOperationInNewPhase(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		for _, kind := range []string{"action", "vote"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				card := addVoteCard(t, f, "Важное")
				path := "/api/boards/" + f.id + "/actions"
				body := f.payload("actions", "Решение после записи")
				wantPhase := model.PhaseReveal
				if kind == "action" {
					f.phase("write")
				} else {
					path = "/api/boards/" + f.id + "/cards/" + card + "/vote"
					body = map[string]any{"voted": true}
					wantPhase = model.PhaseVote
				}
				setTimerEnd(t, f.store, f.id, time.Now().Add(-time.Minute))
				before := f.state().Revision
				f.reopen()
				rec, _ := doMutation(t, f.handler, "POST", path, mutationAuthor, "new-phase-0001", body)
				requireMutationStatus(t, rec, http.StatusOK)
				after := f.state()
				if after.Phase != wantPhase || after.TimerEndsAt != nil || after.Revision != before+1 {
					t.Fatalf("переход и операция должны сохраниться одной версией: %+v", after)
				}
				if kind == "action" && (len(after.Actions) != 1 || len(after.Receipts) != 1) {
					t.Fatal("решение и квитанция не сохранились после перехода в показ")
				}
				if kind == "vote" && after.VotesUsed(mutationAuthor) != 1 {
					t.Fatal("голос не принят после перехода в голосование")
				}
			})
		}
	}
}

func TestRejectedMutationKeepsOnlyExpiryAndDiscardsPartialChanges(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		for _, denial := range []struct {
			name   string
			err    error
			status int
		}{{"нет-прав", model.ErrForbidden, http.StatusForbidden}, {"не-та-фаза", model.ErrPhase, http.StatusConflict}} {
			t.Run(backend+"/"+denial.name, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				rec, _ := doMutation(t, f.handler, "POST", "/api/boards/"+f.id+"/cards", mutationAuthor, "existing-receipt", f.payload("cards", "Карточка"))
				requireMutationStatus(t, rec, http.StatusOK)
				rec, _ = do(t, f.handler, "POST", "/api/boards/"+f.id+"/actions", mutationAuthor, f.payload("actions", "Решение"))
				requireMutationStatus(t, rec, http.StatusOK)
				setTimerEnd(t, f.store, f.id, time.Now().Add(-time.Minute))
				want := f.state()
				want.ExpireTimer(time.Now())
				want.Revision++
				s := &Server{store: f.store, hub: f.hub, log: slog.New(slog.NewTextHandler(io.Discard, nil)), timers: map[string]*phaseTimer{}}
				signals, unsubscribe := f.hub.Subscribe(f.id)
				defer unsubscribe()
				req := mutationRequest(t, "POST", "/api/boards/"+f.id+"/probe", mutationAuthor, "", nil)
				req.SetPathValue("id", f.id)
				rec = httptest.NewRecorder()
				s.mutate(rec, req, func(b *model.Board, pid string) error {
					b.Title = "Не должно сохраниться"
					b.Columns[0].Cards[0].Text = "Частичная правка карточки"
					b.Columns[0].Cards[0].Votes = append(b.Columns[0].Cards[0].Votes, pid)
					b.Actions[0].Text = "Частичная правка решения"
					b.Receipts[pid+":existing-receipt"] = model.MutationReceipt{Operation: "ошибка", Fingerprint: "ошибка"}
					future := time.Now().Add(time.Hour)
					b.TimerEndsAt, b.Phase = &future, model.PhaseWrite
					return denial.err
				})
				requireMutationStatus(t, rec, denial.status)
				got := f.state()
				want.UpdatedAt = got.UpdatedAt
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("вместе с истечением сохранились части отклонённой операции:\nполучено=%+v\nожидается=%+v", got, want)
				}
				requireExpiryNotification(t, signals)
				s.mu.Lock()
				scheduled := len(s.timers)
				s.mu.Unlock()
				if scheduled != 0 {
					t.Fatal("таймер из отклонённого callback попал в планировщик")
				}
			})
		}
	}
}

type notifyWaitingUpdateStore struct {
	store.Store
	started chan struct{}
	once    sync.Once
}

func (s *notifyWaitingUpdateStore) Update(id string, fn func(*model.Board) error) (*model.Board, error) {
	s.once.Do(func() { close(s.started) })
	return s.Store.Update(id, fn)
}

func TestMutationChecksDeadlineAfterWaitingForStore(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			card := addVoteCard(t, f, "Важное")
			f.phase("vote")
			waiting := &notifyWaitingUpdateStore{Store: f.store, started: make(chan struct{})}
			f.serveWith(waiting)
			locked := make(chan struct{})
			deadline := make(chan time.Time, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer func() {
				select {
				case deadline <- time.Now():
				default:
				}
				releaseOnce.Do(func() { close(release) })
			}()
			blockerDone := make(chan error, 1)
			go func() {
				_, err := f.store.Update(f.id, func(b *model.Board) error {
					close(locked)
					at := <-deadline
					b.TimerEndsAt = &at
					<-release
					return nil
				})
				blockerDone <- err
			}()
			select {
			case <-locked:
			case <-time.After(3 * time.Second):
				t.Fatal("хранилище не заблокировано")
			}
			req := mutationRequest(t, "POST", "/api/boards/"+f.id+"/cards/"+card+"/vote", mutationAuthor, "", map[string]any{"voted": true})
			response := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rec := httptest.NewRecorder()
				f.handler.ServeHTTP(rec, req)
				response <- rec
			}()
			select {
			case <-waiting.started:
			case <-time.After(3 * time.Second):
				t.Fatal("запрос не дошёл до Store.Update")
			}
			// Запрос уже начался, но ждёт чужой транзакции. Её дедлайн
			// наступит до освобождения хранилища и выполнения callback запроса.
			at := time.Now().Add(20 * time.Millisecond)
			deadline <- at
			time.Sleep(time.Until(at) + time.Millisecond)
			releaseOnce.Do(func() { close(release) })
			if err := <-blockerDone; err != nil {
				t.Fatal(err)
			}
			select {
			case rec := <-response:
				requireMutationStatus(t, rec, http.StatusConflict)
			case <-time.After(3 * time.Second):
				t.Fatal("запрос не завершился после освобождения хранилища")
			}
			if b := f.state(); b.Phase != model.PhaseDiscuss || b.TimerEndsAt != nil || b.VotesUsed(mutationAuthor) != 0 {
				t.Fatalf("ожидавший транзакции запрос воспользовался старой фазой: %+v", b)
			}
		})
	}
}

func failExpiryWrites(t *testing.T, f *mutationFixture, backend string) func() {
	t.Helper()
	var restore func()
	if backend == "json" {
		if err := os.Mkdir(f.path+".tmp", 0o755); err != nil {
			t.Fatal(err)
		}
		restore = func() {
			if err := os.Remove(f.path + ".tmp"); err != nil {
				t.Error(err)
			}
		}
	} else {
		db, err := sql.Open("sqlite", f.path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TRIGGER fail_expiry BEFORE UPDATE ON boards BEGIN SELECT RAISE(FAIL, 'ошибка сохранения'); END`); err != nil {
			db.Close()
			t.Fatal(err)
		}
		restore = func() {
			if _, err := db.Exec(`DROP TRIGGER fail_expiry`); err != nil {
				t.Error(err)
			}
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		}
	}
	var once sync.Once
	cleanup := func() { once.Do(restore) }
	t.Cleanup(cleanup)
	return cleanup
}

func TestExpiryAndMutationRollbackTogetherOnPersistenceFailure(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			f.phase("write")
			setTimerEnd(t, f.store, f.id, time.Now().Add(-time.Minute))
			before := f.state()
			f.reopen()
			restore := failExpiryWrites(t, f, backend)
			signals, unsubscribe := f.hub.Subscribe(f.id)
			defer unsubscribe()
			path := "/api/boards/" + f.id + "/actions"
			body := f.payload("actions", "Решение после восстановления")
			rec, _ := doMutation(t, f.handler, "POST", path, mutationAuthor, "expiry-retry-01", body)
			requireMutationStatus(t, rec, http.StatusInternalServerError)
			if got := f.state(); !reflect.DeepEqual(got, before) {
				t.Fatalf("сбой сохранения не откатил фазу, решение и квитанцию: %+v", got)
			}
			select {
			case <-signals:
				t.Fatal("несохранённое истечение оповестило подписчиков")
			default:
			}
			restore()
			rec, _ = doMutation(t, f.handler, "POST", path, mutationAuthor, "expiry-retry-01", body)
			requireMutationStatus(t, rec, http.StatusOK)
			got := f.state()
			if got.Phase != model.PhaseReveal || got.TimerEndsAt != nil || len(got.Actions) != 1 || len(got.Receipts) != 1 || got.Revision != before.Revision+1 {
				t.Fatalf("после восстановления переход и запрос должны примениться ровно раз: %+v", got)
			}
			requireExpiryNotification(t, signals)
		})
	}
}

func TestIdempotentReplayCommitsOnlyPendingExpiry(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		for _, kind := range []string{"cards", "actions"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				path := "/api/boards/" + f.id + "/" + kind
				body := f.payload(kind, "Повтор без дубликата")
				rec, _ := doMutation(t, f.handler, "POST", path, mutationAuthor, "expiry-replay-01", body)
				requireMutationStatus(t, rec, http.StatusOK)
				setTimerEnd(t, f.store, f.id, time.Now().Add(-time.Minute))
				before := f.state().Revision
				f.reopen()
				signals, unsubscribe := f.hub.Subscribe(f.id)
				defer unsubscribe()
				rec, view := doMutation(t, f.handler, "POST", path, mutationAuthor, "expiry-replay-01", body)
				requireMutationStatus(t, rec, http.StatusOK)
				if b := f.state(); b.Phase != model.PhaseVote || b.TimerEndsAt != nil || b.Revision != before+1 || len(mutationItems(view, kind)) != 1 || len(b.Receipts) != 1 {
					t.Fatalf("повтор должен сохранить только истечение: %+v", b)
				}
				requireExpiryNotification(t, signals)
				rec, _ = doMutation(t, f.handler, "POST", path, mutationAuthor, "expiry-replay-01", body)
				requireMutationStatus(t, rec, http.StatusOK)
				if f.state().Revision != before+1 {
					t.Fatal("повтор без истечения изменил версию")
				}
			})
		}
	}
}
