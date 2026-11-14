package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"retro/internal/model"
)

func writingJoin(t *testing.T, f *mutationFixture, actor string) {
	t.Helper()
	round := f.state().View(actor).Writing.Round
	rec, _ := do(t, f.handler, "POST", "/api/boards/"+f.id+"/writing/join", actor, map[string]any{"round": round})
	requireMutationStatus(t, rec, http.StatusOK)
}

func writingCommand(f *mutationFixture, actor string, ready bool) map[string]any {
	w := f.state().View(actor).Writing
	return map[string]any{"ready": ready, "round": w.Round, "revision": w.Revision}
}

func TestWritingReadyPersistsAndReplaysAfterRestart(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			f.phase("write")
			writingJoin(t, f, mutationAuthor)
			path := "/api/boards/" + f.id + "/writing/ready"
			body := writingCommand(f, mutationOwner, true)
			f.handler.ServeHTTP(&lostMutationResponse{headers: make(http.Header)}, mutationRequest(t, "POST", path, mutationOwner, "", body))
			before := f.state()
			f.reopen()
			rec, view := do(t, f.handler, "POST", path, mutationOwner, body)
			requireMutationStatus(t, rec, http.StatusOK)
			if got := view["writing"].(map[string]any); got["ready"] != float64(1) || got["mine"] != true || got["participants"] != float64(2) {
				t.Fatalf("готовность потерялась при рестарте: %v", got)
			}
			writingJoin(t, f, mutationOwner)
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("повтор нажатия или входа изменил состояние доски")
			}
			rec, _ = do(t, f.handler, "POST", "/api/boards/"+f.id+"/timer", mutationOwner, map[string]any{"minutes": 5})
			requireMutationStatus(t, rec, http.StatusOK)
			body = writingCommand(f, mutationAuthor, true)
			rec, _ = do(t, f.handler, "POST", path, mutationAuthor, body)
			requireMutationStatus(t, rec, http.StatusOK)
			after := f.state()
			if after.Phase != model.PhaseReveal || after.TimerEndsAt != nil {
				t.Fatal("последняя готовность не открыла карточки или оставила таймер")
			}
			f.reopen()
			rec, _ = do(t, f.handler, "POST", path, mutationAuthor, body)
			requireMutationStatus(t, rec, http.StatusOK)
			if !reflect.DeepEqual(after, f.state()) {
				t.Fatal("повтор последнего нажатия после рестарта изменил фазу или версию")
			}
		})
	}
}

func TestWritingFifteenConcurrentParticipantsAdvanceOnce(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			f.phase("write")
			actors := []string{mutationOwner}
			for i := 1; i < 15; i++ {
				actor := fmt.Sprintf("participant-%02d", i)
				writingJoin(t, f, actor)
				actors = append(actors, actor)
			}
			before := f.state()
			var wg sync.WaitGroup
			replies := make([]*httptest.ResponseRecorder, len(actors)*2)
			for i := range replies {
				actor := actors[i/2]
				req := mutationRequest(t, "POST", "/api/boards/"+f.id+"/writing/ready", actor, "", map[string]any{"ready": true, "round": before.Writing.Round, "revision": 0})
				replies[i] = httptest.NewRecorder()
				wg.Add(1)
				go func() {
					defer wg.Done()
					f.handler.ServeHTTP(replies[i], req)
				}()
			}
			wg.Wait()
			for _, rec := range replies {
				requireMutationStatus(t, rec, http.StatusOK)
			}
			after := f.state()
			if after.Phase != model.PhaseReveal || after.Revision != before.Revision+15 {
				t.Fatalf("30 запросов от 15 участников должны дать 15 отметок и один переход: %+v", after)
			}
		})
	}
}

func TestWritingRejectsMalformedRequestsBeforeTimerMutation(t *testing.T) {
	f := newMutationFixture(t, "db")
	f.phase("write")
	setTimerEnd(t, f.store, f.id, time.Now().Add(-time.Minute))
	f.reopen()
	before := f.state()
	signals, unsubscribe := f.hub.Subscribe(f.id)
	defer unsubscribe()
	for _, payload := range []string{`{}`, `null`, `{"ready":null,"round":2,"revision":0}`, `{"ready":true,"revision":0}`, `{"ready":true,"round":2}`, `{"ready":"true","round":2,"revision":0}`, `{"ready":true,"round":2,"revision":-1}`, `{"ready":true,"round":2,"revision":0} null`} {
		rec := rawJSONRequest(f.handler, "POST", "/api/boards/"+f.id+"/writing/ready", payload, "")
		requireMutationStatus(t, rec, http.StatusBadRequest)
		requireRequestPreservedBoard(t, f, before, signals)
	}
	for _, payload := range []string{`{}`, `null`, `{"round":null}`, `{"round":2} {}`} {
		rec := rawJSONRequest(f.handler, "POST", "/api/boards/"+f.id+"/writing/join", payload, "")
		requireMutationStatus(t, rec, http.StatusBadRequest)
		requireRequestPreservedBoard(t, f, before, signals)
	}
	rec, _ := do(t, f.handler, "POST", "/api/boards/"+f.id+"/writing/ready", mutationOwner, writingCommand(f, mutationOwner, true))
	requireMutationStatus(t, rec, http.StatusConflict)
	if after := f.state(); after.Phase != model.PhaseReveal || after.TimerEndsAt != nil || after.Revision != before.Revision+1 {
		t.Fatal("истёкший таймер и нажатие должны перевести фазу только один раз")
	}
}

func TestWritingLastReadyRollsBackOnStorageFailure(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			f.phase("write")
			before := f.state()
			failing := &failAfterMutationStore{Store: f.store}
			failing.fail.Store(true)
			f.serveWith(failing)
			body := writingCommand(f, mutationOwner, true)
			path := "/api/boards/" + f.id + "/writing/ready"
			rec, _ := do(t, f.handler, "POST", path, mutationOwner, body)
			requireMutationStatus(t, rec, http.StatusInternalServerError)
			if !reflect.DeepEqual(before, f.state()) {
				t.Fatal("ошибка записи сохранила часть готовности или перехода")
			}
			rec, _ = do(t, f.handler, "POST", path, mutationOwner, body)
			requireMutationStatus(t, rec, http.StatusOK)
			if f.state().Phase != model.PhaseReveal {
				t.Fatal("повтор после ошибки должен завершить этап")
			}
		})
	}
}
