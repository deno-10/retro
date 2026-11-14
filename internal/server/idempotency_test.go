package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"

	"retro/internal/hub"
	"retro/internal/model"
	"retro/internal/store"
)

const mutationOwner = "owner-00000001"
const mutationAuthor = "author-00000001"

type mutationFixture struct {
	t       *testing.T
	path    string
	store   store.Store
	hub     *hub.Hub
	handler http.Handler
	id      string
	column  string
}

func newMutationFixture(t *testing.T, backend string) *mutationFixture {
	t.Helper()
	f := &mutationFixture{t: t, path: filepath.Join(t.TempDir(), "boards."+backend)}
	f.reopen()
	t.Cleanup(f.close)
	rec, view := do(t, f.handler, "POST", "/api/boards", mutationOwner, map[string]any{"title": "Повторы и конфликты"})
	requireMutationStatus(t, rec, http.StatusCreated)
	f.id = view["id"].(string)
	f.column = view["columns"].([]any)[0].(map[string]any)["id"].(string)
	f.phase("reveal")
	return f
}

func (f *mutationFixture) close() {
	if f.hub != nil {
		f.hub.Close()
	}
	if f.store != nil {
		if err := f.store.Close(); err != nil {
			f.t.Error(err)
		}
		f.store = nil
	}
}

func (f *mutationFixture) reopen() {
	f.t.Helper()
	f.close()
	st, err := store.Open(f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.store = st
	f.serveWith(st)
}

func (f *mutationFixture) serveWith(st store.Store) {
	if f.hub != nil {
		f.hub.Close()
	}
	f.hub = hub.New()
	f.handler = New(st, f.hub, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}},
		slog.New(slog.NewTextHandler(io.Discard, nil)), testOptions())
}

func (f *mutationFixture) phase(phase string) {
	f.t.Helper()
	rec, _ := do(f.t, f.handler, "POST", "/api/boards/"+f.id+"/phase", mutationOwner, map[string]any{"phase": phase})
	requireMutationStatus(f.t, rec, http.StatusOK)
}

func (f *mutationFixture) state() *model.Board {
	f.t.Helper()
	b, err := f.store.Get(f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *mutationFixture) payload(kind, text string) map[string]any {
	if kind == "cards" {
		return map[string]any{"columnId": f.column, "text": text, "author": "Анна"}
	}
	return map[string]any{"text": text, "assignee": "Анна"}
}

func mutationItems(view map[string]any, kind string) []any {
	if kind == "cards" {
		return view["columns"].([]any)[0].(map[string]any)["cards"].([]any)
	}
	return view["actions"].([]any)
}

func mutationRequest(t *testing.T, method, path, actor, key string, body any) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	testGuest(req, actor)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	return req
}

func doMutation(t *testing.T, h http.Handler, method, path, actor, key string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, mutationRequest(t, method, path, actor, key, body))
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return rec, view
}

func requireMutationStatus(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("HTTP %d, ожидается %d: %s", rec.Code, status, rec.Body.String())
	}
}

type lostMutationResponse struct{ headers http.Header }

func (w *lostMutationResponse) Header() http.Header       { return w.headers }
func (w *lostMutationResponse) WriteHeader(int)           {}
func (w *lostMutationResponse) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCreateRetrySurvivesLostResponseRestartAndDeletion(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		for _, kind := range []string{"cards", "actions"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				path := "/api/boards/" + f.id + "/" + kind
				body := f.payload(kind, "Исходная запись")
				const key = "creation-00000001"
				f.handler.ServeHTTP(&lostMutationResponse{headers: http.Header{}},
					mutationRequest(t, "POST", path, mutationAuthor, key, body))
				before := f.state()
				rec, view := doMutation(t, f.handler, "POST", path, mutationAuthor, key, body)
				requireMutationStatus(t, rec, http.StatusOK)
				items := mutationItems(view, kind)
				if len(items) != 1 || view["receipts"] != nil {
					t.Fatalf("повтор должен вернуть одну запись без квитанций: %v", view)
				}
				if after := f.state(); after.Revision != before.Revision || !after.UpdatedAt.Equal(before.UpdatedAt) {
					t.Fatal("повтор без новой мутации изменил версию доски")
				}
				item := items[0].(map[string]any)
				itemPath := path + "/" + item["id"].(string)
				rec, _ = do(t, f.handler, "PATCH", itemPath, mutationOwner,
					map[string]any{"text": "Обновлённая запись", "assignee": "Борис", "revision": item["revision"]})
				requireMutationStatus(t, rec, http.StatusOK)
				blockedPhase := "vote"
				if kind == "actions" {
					blockedPhase = "write"
				}
				f.phase(blockedPhase) // Первоначальное создание теперь запрещено.
				f.reopen()
				rec, view = doMutation(t, f.handler, "POST", path, mutationAuthor, key, body)
				requireMutationStatus(t, rec, http.StatusOK)
				items = mutationItems(view, kind)
				if len(items) != 1 || items[0].(map[string]any)["text"] != "Обновлённая запись" {
					t.Fatalf("повтор после перезапуска должен вернуть актуальное представление: %v", items)
				}
				rec, _ = do(t, f.handler, "DELETE", itemPath, mutationOwner, nil)
				requireMutationStatus(t, rec, http.StatusOK)
				f.reopen()
				rec, view = doMutation(t, f.handler, "POST", path, mutationAuthor, key, body)
				requireMutationStatus(t, rec, http.StatusOK)
				if len(mutationItems(view, kind)) != 0 || len(f.state().Receipts) != 1 {
					t.Fatal("поздний повтор воскресил удалённую запись или потерял квитанцию")
				}
			})
		}
	}
}

func TestConcurrentCreateRetriesApplyOnce(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		for _, kind := range []string{"cards", "actions"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				before := f.state().Revision
				path := "/api/boards/" + f.id + "/" + kind
				body := f.payload(kind, "Одновременный повтор")
				start := make(chan struct{})
				results := make(chan *httptest.ResponseRecorder, 8)
				var wg sync.WaitGroup
				for range cap(results) {
					req := mutationRequest(t, "POST", path, mutationAuthor, "concurrent-0001", body)
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						rec := httptest.NewRecorder()
						f.handler.ServeHTTP(rec, req)
						results <- rec
					}()
				}
				close(start)
				wg.Wait()
				close(results)
				for rec := range results {
					requireMutationStatus(t, rec, http.StatusOK)
				}
				b := f.state()
				if len(b.Receipts) != 1 || b.Revision != before+1 {
					t.Fatalf("ожидалось одно сохранение: квитанций=%d, revision=%d, исходная=%d", len(b.Receipts), b.Revision, before)
				}
				_, view := do(t, f.handler, "GET", "/api/boards/"+f.id, mutationAuthor, nil)
				if len(mutationItems(view, kind)) != 1 {
					t.Fatal("одновременные повторы создали несколько записей")
				}
			})
		}
	}
}

func TestCreateIdempotencyKeyScopeAndConflicts(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		for _, kind := range []string{"cards", "actions"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				path := "/api/boards/" + f.id + "/" + kind
				const key = "shared-key-0001"
				body := f.payload(kind, "Исходная запись")
				rec, _ := doMutation(t, f.handler, "POST", path, mutationAuthor, key, body)
				requireMutationStatus(t, rec, http.StatusOK)
				rec, _ = doMutation(t, f.handler, "POST", path, mutationAuthor, key, f.payload(kind, "Другой текст"))
				requireMutationStatus(t, rec, http.StatusConflict)
				otherKind := "actions"
				if kind == otherKind {
					otherKind = "cards"
				}
				rec, _ = doMutation(t, f.handler, "POST", "/api/boards/"+f.id+"/"+otherKind, mutationAuthor, key, f.payload(otherKind, "Исходная запись"))
				requireMutationStatus(t, rec, http.StatusConflict)
				rec, view := doMutation(t, f.handler, "POST", path, mutationOwner, key, body)
				requireMutationStatus(t, rec, http.StatusOK)
				if len(mutationItems(view, kind)) != 2 || len(f.state().Receipts) != 2 {
					t.Fatal("ключи разных участников не должны конфликтовать")
				}
			})
		}
	}
}

func TestCreateRetryFingerprintsTypedPayload(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		for _, kind := range []string{"cards", "actions"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				path := "/api/boards/" + f.id + "/" + kind
				body := map[string]any{"text": "Без подписи"}
				if kind == "cards" {
					body["columnId"] = f.column
				}
				rec, _ := doMutation(t, f.handler, "POST", path, mutationAuthor, "typed-payload-01", body)
				requireMutationStatus(t, rec, http.StatusOK)
				if kind == "cards" {
					body["author"] = ""
				} else {
					body["assignee"] = ""
				}
				rec, view := doMutation(t, f.handler, "POST", path, mutationAuthor, "typed-payload-01", body)
				requireMutationStatus(t, rec, http.StatusOK)
				if len(mutationItems(view, kind)) != 1 {
					t.Fatal("пропущенное и явно пустое необязательное поле не должны создавать разные операции")
				}
			})
		}
	}
}

type failAfterMutationStore struct {
	store.Store
	fail atomic.Bool
}

func (s *failAfterMutationStore) Update(id string, fn func(*model.Board) error) (*model.Board, error) {
	return s.Store.Update(id, func(b *model.Board) error {
		if err := fn(b); err != nil {
			return err
		}
		if s.fail.Swap(false) {
			return errors.New("имитация сбоя сохранения")
		}
		return nil
	})
}

func TestFailedCreateDoesNotConsumeIdempotencyKey(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		for _, kind := range []string{"cards", "actions"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				path := "/api/boards/" + f.id + "/" + kind
				body := f.payload(kind, "Запись после ошибки")
				bad := f.payload(kind, "Запись после ошибки")
				want := http.StatusNotFound
				if kind == "cards" {
					bad["columnId"] = "missing-column"
				} else {
					f.phase("write")
					want = http.StatusConflict
				}
				rec, _ := doMutation(t, f.handler, "POST", path, mutationAuthor, "retry-after-error", bad)
				requireMutationStatus(t, rec, want)
				if len(f.state().Receipts) != 0 {
					t.Fatal("ошибка бизнес-правила сохранила квитанцию")
				}
				f.phase("reveal")
				failing := &failAfterMutationStore{Store: f.store}
				failing.fail.Store(true)
				f.serveWith(failing)
				rec, _ = doMutation(t, f.handler, "POST", path, mutationAuthor, "retry-after-error", body)
				requireMutationStatus(t, rec, http.StatusInternalServerError)
				b := f.state()
				if len(b.Receipts) != 0 || len(b.Columns[0].Cards) != 0 || len(b.Actions) != 0 {
					t.Fatal("откат должен убрать и запись, и квитанцию")
				}
				rec, view := doMutation(t, f.handler, "POST", path, mutationAuthor, "retry-after-error", body)
				requireMutationStatus(t, rec, http.StatusOK)
				if len(mutationItems(view, kind)) != 1 || len(f.state().Receipts) != 1 {
					t.Fatal("после ошибки тот же ключ должен создать одну запись")
				}
			})
		}
	}
}

func TestCreateIdempotencyKeyValidationAndLegacyRequests(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			path := "/api/boards/" + f.id + "/cards"
			body := f.payload("cards", "Совместимость")
			for _, key := range []string{"", "short", strings.Repeat("a", 65), "invalid_key", "русский-ключ"} {
				req := mutationRequest(t, "POST", path, mutationAuthor, "", body)
				req.Header.Set("Idempotency-Key", key)
				rec := httptest.NewRecorder()
				f.handler.ServeHTTP(rec, req)
				requireMutationStatus(t, rec, http.StatusBadRequest)
			}
			if len(f.state().Receipts) != 0 {
				t.Fatal("некорректный ключ сохранил квитанцию")
			}
			for range 2 {
				rec, _ := doMutation(t, f.handler, "POST", path, mutationAuthor, "", body)
				requireMutationStatus(t, rec, http.StatusOK)
			}
			if b := f.state(); len(b.Columns[0].Cards) != 2 || len(b.Receipts) != 0 {
				t.Fatal("запросы без ключа должны сохранять прежнюю семантику")
			}
		})
	}
}
