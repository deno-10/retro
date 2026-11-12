package server

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"retro/internal/hub"
	"retro/internal/jira"
	"retro/internal/model"
	"retro/internal/store"
)

const jiraOwner = "owner-00000001"

func jiraHandler(t *testing.T, st store.Store, client *jira.Client) http.Handler {
	t.Helper()
	h := hub.New()
	t.Cleanup(h.Close)
	options := DefaultOptions()
	options.Jira = client
	return New(st, h, fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}},
		slog.New(slog.NewTextHandler(io.Discard, nil)), options)
}

func jiraFixture(t *testing.T, filename string, remote http.HandlerFunc) (store.Store, *model.Board, *jira.Client, string) {
	t.Helper()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/api/2/myself" {
			_, _ = w.Write([]byte(`{"name":"test-user","accountId":"test-account"}`))
			return
		}
		remote(w, r)
	}))
	t.Cleanup(mock.Close)
	client, err := jira.New(jira.Config{BaseURL: mock.URL, Project: "TEAM", IssueType: "10001", Token: "only-for-test"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), filename)
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b, _ := model.NewBoard("Ретро", "classic", jiraOwner)
	b.Phase = model.PhaseDiscuss
	for _, text := range []string{"Выбранное решение", "Не выбранное решение"} {
		if _, err := b.AddAction(text, "Анна", jiraOwner); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Create(b); err != nil {
		t.Fatal(err)
	}
	return st, b, client, path
}

func jiraState(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	if out["actions"] == nil {
		t.Fatalf("нет доски в ответе: %v", out)
	}
	return out["actions"].([]any)[0].(map[string]any)["jira"].(map[string]any)
}

func TestJiraSelectedOnlyConcurrentRequestsAndRestart(t *testing.T) {
	for _, filename := range []string{"boards.json", "retro.db"} {
		t.Run(filename, func(t *testing.T) {
			var calls atomic.Int32
			entered, release := make(chan struct{}), make(chan struct{})
			st, b, client, path := jiraFixture(t, filename, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				close(entered)
				<-release
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"key":"TEAM-7"}`))
			})
			h := jiraHandler(t, st, client)
			endpoint := "/api/boards/" + b.ID + "/actions/" + b.Actions[0].ID + "/jira"
			finished := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rec, _ := do(t, h, "POST", endpoint, jiraOwner, map[string]any{"revision": 1})
				finished <- rec
			}()
			<-entered
			rec, out := do(t, h, "POST", endpoint, jiraOwner, map[string]any{"revision": 1})
			if rec.Code != 200 || jiraState(t, out)["status"] != "pending" || calls.Load() != 1 {
				t.Fatalf("дубликат: %d %s", rec.Code, rec.Body)
			}
			// Запрос Jira не удерживает транзакцию: другая карточка записывается.
			rec, _ = do(t, h, "POST", "/api/boards/"+b.ID+"/actions", jiraOwner, map[string]string{"text": "Параллельное решение"})
			if rec.Code != 200 {
				t.Fatal("запрос Jira заблокировал доску")
			}
			rec, _ = do(t, h, "DELETE", strings.TrimSuffix(endpoint, "/jira"), jiraOwner, nil)
			if rec.Code != 409 {
				t.Fatal("удалена отправляемая задача")
			}
			rec, _ = do(t, h, "POST", "/api/boards/"+b.ID+"/next", jiraOwner, map[string]string{})
			if rec.Code != 409 {
				t.Fatal("следующее ретро потеряло связь с отправкой")
			}
			close(release)
			if rec := <-finished; rec.Code != 200 {
				t.Fatalf("создание: %d %s", rec.Code, rec.Body)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			h = jiraHandler(t, reopened, client)
			rec, out = do(t, h, "POST", endpoint, jiraOwner, map[string]any{"revision": 1})
			if rec.Code != 200 || jiraState(t, out)["key"] != "TEAM-7" || calls.Load() != 1 {
				t.Fatal("перезапуск повторно создал задачу")
			}
			if out["actions"].([]any)[1].(map[string]any)["jira"] != nil {
				t.Fatal("в Jira отправлено невыбранное решение")
			}
			if strings.Contains(rec.Body.String(), "only-for-test") || strings.Contains(rec.Body.String(), `"target"`) {
				t.Fatal("раскрыта внутренняя конфигурация")
			}
			rec, out = do(t, h, "POST", "/api/boards/"+b.ID+"/next", jiraOwner, map[string]string{})
			if rec.Code != 201 || jiraState(t, out)["key"] != "TEAM-7" {
				t.Fatal("ссылка не перенесена в следующее ретро")
			}
		})
	}
}

type failJiraStore struct {
	store.Store
	failAt int32
	calls  atomic.Int32
}

func (s *failJiraStore) Update(id string, fn func(*model.Board) error) (*model.Board, error) {
	if s.calls.Add(1) == s.failAt {
		return nil, errors.New("имитация сбоя записи")
	}
	return s.Store.Update(id, fn)
}

func TestJiraStorageFailureBeforeAndAfterRemoteCreation(t *testing.T) {
	for _, filename := range []string{"boards.json", "retro.db"} {
		for _, failAt := range []int32{1, 2} {
			t.Run(filename+string(rune('0'+failAt)), func(t *testing.T) {
				var creates atomic.Int32
				var label string
				st, b, client, path := jiraFixture(t, filename, func(w http.ResponseWriter, r *http.Request) {
					if r.Method == "POST" {
						creates.Add(1)
						var body struct {
							Fields struct {
								Labels []string `json:"labels"`
							} `json:"fields"`
						}
						_ = json.NewDecoder(r.Body).Decode(&body)
						label = body.Fields.Labels[1]
						w.WriteHeader(201)
						_, _ = w.Write([]byte(`{"key":"TEAM-8"}`))
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"issues": []any{map[string]any{"key": "TEAM-8", "fields": map[string]any{"labels": []string{label}}}}})
				})
				h := jiraHandler(t, &failJiraStore{Store: st, failAt: failAt}, client)
				endpoint := "/api/boards/" + b.ID + "/actions/" + b.Actions[0].ID + "/jira"
				rec, _ := do(t, h, "POST", endpoint, jiraOwner, map[string]int{"revision": 1})
				if rec.Code != 500 {
					t.Fatalf("ожидался сбой записи: %s", rec.Body)
				}
				if failAt == 1 {
					if creates.Load() != 0 {
						t.Fatal("запрос отправлен до сохранения резервации")
					}
					return
				}
				_ = st.Close()
				reopened, err := store.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				h = jiraHandler(t, reopened, client)
				rec, out := do(t, h, "POST", endpoint, jiraOwner, map[string]int{"revision": 1})
				if rec.Code != 200 || jiraState(t, out)["status"] != "pending" || creates.Load() != 1 {
					t.Fatal("сбой сохранения вызвал повтор создания")
				}
				rec, out = do(t, h, "POST", endpoint+"/check", jiraOwner, map[string]any{})
				if rec.Code != 200 || jiraState(t, out)["key"] != "TEAM-8" || creates.Load() != 1 {
					t.Fatalf("ссылка не восстановилась: %s", rec.Body)
				}
			})
		}
	}
}

func TestJiraValidationAndDefiniteFailureRetry(t *testing.T) {
	var creates atomic.Int32
	var status atomic.Int32
	status.Store(400)
	st, b, client, _ := jiraFixture(t, "retro.db", func(w http.ResponseWriter, r *http.Request) {
		creates.Add(1)
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(`{"key":"TEAM-9"}`))
	})
	h := jiraHandler(t, st, client)
	endpoint := "/api/boards/" + b.ID + "/actions/" + b.Actions[0].ID + "/jira"
	for _, tc := range []struct {
		actor   string
		payload any
		code    int
	}{
		{"other-00000001", map[string]int{"revision": 1}, 403},
		{jiraOwner, map[string]int{"revision": 0}, 409},
		{jiraOwner, map[string]any{}, 400},
	} {
		rec, _ := do(t, h, "POST", endpoint, tc.actor, tc.payload)
		if rec.Code != tc.code {
			t.Fatalf("проверка входа: %d %s", rec.Code, rec.Body)
		}
	}
	req := httptest.NewRequest("POST", endpoint, strings.NewReader(`{"revision":1} {}`))
	req.Header.Set("X-Participant", jiraOwner)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 || creates.Load() != 0 {
		t.Fatal("некорректные данные вызвали создание")
	}
	_, out := do(t, h, "POST", endpoint, jiraOwner, map[string]int{"revision": 1})
	if jiraState(t, out)["status"] != "failed" {
		t.Fatal("явный отказ нельзя повторить")
	}
	revision := out["actions"].([]any)[0].(map[string]any)["revision"]
	status.Store(201)
	rec, out = do(t, h, "POST", endpoint, jiraOwner, map[string]any{"revision": revision})
	if rec.Code != 200 || jiraState(t, out)["status"] != "created" || creates.Load() != 2 {
		t.Fatal("повтор после исправления настроек не сработал")
	}
}

func TestJiraUnknownOutcomeIsNotRetriedAndTimerIsHonoured(t *testing.T) {
	var creates atomic.Int32
	st, b, client, _ := jiraFixture(t, "retro.db", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			creates.Add(1)
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"key":`))
			return
		}
		_, _ = w.Write([]byte(`{"issues":[]}`))
	})
	_, err := st.Update(b.ID, func(b *model.Board) error {
		b.Phase = model.PhaseVote
		past := time.Now().Add(-time.Second)
		b.TimerEndsAt = &past
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h := jiraHandler(t, st, client)
	endpoint := "/api/boards/" + b.ID + "/actions/" + b.Actions[0].ID + "/jira"
	rec, out := do(t, h, "POST", endpoint, jiraOwner, map[string]int{"revision": 1})
	if rec.Code != 200 || out["phase"] != "discuss" || jiraState(t, out)["status"] != "unknown" {
		t.Fatalf("таймер/результат: %s", rec.Body)
	}
	_, _ = do(t, h, "POST", endpoint, jiraOwner, map[string]int{"revision": 1})
	rec, _ = do(t, h, "POST", endpoint+"/check", jiraOwner, map[string]any{})
	if rec.Code != 409 || creates.Load() != 1 {
		t.Fatal("пустой поиск или повтор создал дубликат")
	}
	current, _ := st.Get(b.ID)
	if current.Actions[0].Jira.Status != "unknown" {
		t.Fatal("пустой поиск снял защиту от дубликата")
	}
}
