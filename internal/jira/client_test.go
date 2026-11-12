package jira

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func testJiraServer(handler http.Handler) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/rest/api/2/myself") {
			_, _ = w.Write([]byte(`{"name":"test-user","accountId":"test-account"}`))
			return
		}
		handler.ServeHTTP(w, r)
	}))
}

func configFor(url string) Config {
	return Config{BaseURL: url, Project: "TEAM", IssueType: "10001", Token: "test-secret"}
}

func TestCreatePayloadAndAuthentication(t *testing.T) {
	for _, auth := range []string{"bearer", "basic"} {
		t.Run(auth, func(t *testing.T) {
			text := strings.Repeat("Ускорить проверку кода. ", 35)
			server := testJiraServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/jira/rest/api/2/issue" {
					t.Errorf("неверный адрес: %s %s", r.Method, r.URL)
				}
				if auth == "basic" {
					u, p, ok := r.BasicAuth()
					if !ok || u != "test@example.org" || p != "test-secret" {
						t.Error("неверная Basic-авторизация")
					}
				} else if r.Header.Get("Authorization") != "Bearer test-secret" {
					t.Error("неверная Bearer-авторизация")
				}
				var data struct {
					Fields map[string]any `json:"fields"`
				}
				if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
					t.Error(err)
					return
				}
				f := data.Fields
				if utf8.RuneCountInString(f["summary"].(string)) > 240 || !strings.Contains(f["description"].(string), text) {
					t.Error("длинный текст потерян")
				}
				for _, part := range []string{"Анна", "Спринт 7", "https://retro.example.org/b/board1234", "retro-0123456789abcdef"} {
					if !strings.Contains(f["description"].(string), part) {
						t.Errorf("в описании нет %s", part)
					}
				}
				if _, ok := f["assignee"]; ok {
					t.Error("свободное имя нельзя отправлять как Jira assignee")
				}
				if f["customfield_123"] != "Команда" || len(f["labels"].([]any)) != 2 {
					t.Error("пропали метки или обязательное поле")
				}
				w.WriteHeader(201)
				_, _ = w.Write([]byte(`{"key":"TEAM-42","self":"https://untrusted.example/secret"}`))
			}))
			defer server.Close()
			config := configFor(server.URL + "/jira/")
			config.AuthMode, config.User, config.PublicURL = auth, "test@example.org", "https://retro.example.org/"
			config.ExtraFields = map[string]json.RawMessage{"customfield_123": json.RawMessage(`"Команда"`)}
			client, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			issue, failure := client.Create(context.Background(), Input{Text: text, Assignee: "Анна", BoardTitle: "Спринт 7", BoardID: "board1234", Label: "retro-0123456789abcdef"})
			if failure != nil || issue.Key != "TEAM-42" || issue.URL != server.URL+"/jira/browse/TEAM-42" {
				t.Fatalf("результат: %+v, %v", issue, failure)
			}
		})
	}
}

func TestCreateNeverRetriesUnknownOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		uncertain bool
	}{
		{"отказ полей", 400, `{"errorMessages":["test-secret"]}`, false},
		{"нет доступа", 403, "test-secret", false},
		{"сервер упал", 500, "test-secret", true},
		{"лимит прокси", 429, "test-secret", true},
		{"редирект", 302, "", true},
		{"оборванный ответ", 201, `{"key":`, true},
		{"чужой ключ", 201, `{"key":"OTHER-1"}`, true},
		{"вредоносный ключ", 201, `{"key":"javascript:alert(1)"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := testJiraServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/must-not-follow")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, _ := New(configFor(server.URL))
			_, err := client.Create(context.Background(), Input{Text: "Решение", Label: "retro-0123456789abcdef"})
			if err == nil || err.Uncertain != tc.uncertain || strings.Contains(err.Error(), "test-secret") || calls != 1 {
				t.Fatalf("небезопасный результат: %v; вызовов: %d", err, calls)
			}
		})
	}
}

func TestTimeoutIncludesResponseBody(t *testing.T) {
	server := testJiraServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := New(configFor(server.URL))
	client.http.Timeout = 30 * time.Millisecond
	_, err := client.Create(context.Background(), Input{Text: "Решение", Label: "retro-0123456789abcdef"})
	if err == nil || !err.Uncertain {
		t.Fatal("обрыв тела ответа должен оставить неопределённый результат")
	}
}

func TestFindChecksLabelAndUsesCurrentCloudSearch(t *testing.T) {
	for _, deployment := range []string{"cloud", "datacenter"} {
		t.Run(deployment, func(t *testing.T) {
			body := `{"issues":[{"key":"TEAM-42","fields":{"labels":["retro-0123456789abcdef"]}}]}`
			server := testJiraServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := "/rest/api/2/search"
				if deployment == "cloud" {
					want = "/rest/api/3/search/jql"
				}
				if r.Method != "GET" || r.URL.Path != want || !strings.Contains(r.URL.Query().Get("jql"), `labels = "retro-0123456789abcdef"`) {
					t.Error("неверный поиск")
				}
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			config := configFor(server.URL)
			config.Deployment = deployment
			client, _ := New(config)
			issue, err := client.Find(context.Background(), "retro-0123456789abcdef")
			if err != nil || issue.Key != "TEAM-42" {
				t.Fatalf("задача не восстановлена: %v", err)
			}
			for _, invalid := range []string{`{"issues":[]}`, `{}`, `{"issues":[{"key":"TEAM-42","fields":{"labels":["другая"]}}]}`, `{"issues":[{},{}]}`} {
				body = invalid
				if _, err := client.Find(context.Background(), "retro-0123456789abcdef"); err == nil {
					t.Fatalf("опасный результат принят: %s", invalid)
				}
			}
		})
	}
}

func TestConfigAndSecretFile(t *testing.T) {
	for _, key := range []string{"JIRA_BASE_URL", "JIRA_DEPLOYMENT", "JIRA_PROJECT_KEY", "JIRA_ISSUE_TYPE_ID", "JIRA_AUTH_MODE", "JIRA_USER", "JIRA_TOKEN", "JIRA_TOKEN_FILE", "JIRA_EXTRA_FIELDS", "RETRO_PUBLIC_URL"} {
		t.Setenv(key, "")
	}
	if client, err := FromEnv(); err != nil || client != nil {
		t.Fatal("без настроек интеграция должна быть выключена")
	}
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("test-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JIRA_BASE_URL", "https://jira.example.org")
	t.Setenv("JIRA_PROJECT_KEY", "TEAM")
	t.Setenv("JIRA_ISSUE_TYPE_ID", "10001")
	t.Setenv("JIRA_TOKEN_FILE", path)
	if _, err := FromEnv(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JIRA_TOKEN", "другой токен")
	if _, err := FromEnv(); err == nil {
		t.Fatal("нельзя принять два источника токена")
	}
	for _, address := range []string{"http://jira.example.org", "https://user:secret@jira.example.org", "https://jira.example.org?token=secret", "javascript:alert(1)"} {
		if _, err := New(configFor(address)); err == nil {
			t.Errorf("принят небезопасный адрес %s", address)
		}
	}
}

func TestCloudReporterAndSafePreflightFailure(t *testing.T) {
	var reporter map[string]string
	posts := 0
	allow := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/api/2/myself" {
			if !allow {
				w.WriteHeader(401)
				_, _ = w.Write([]byte("секрет из ответа"))
				return
			}
			_, _ = w.Write([]byte(`{"accountId":"cloud-account"}`))
			return
		}
		posts++
		var body struct {
			Fields struct {
				Reporter map[string]string `json:"reporter"`
			} `json:"fields"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		reporter = body.Fields.Reporter
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"key":"TEAM-1"}`))
	}))
	defer server.Close()
	cfg := configFor(server.URL)
	cfg.Deployment = "cloud"
	client, _ := New(cfg)
	input := Input{Text: "Решение", Label: "retro-0123456789abcdef"}
	_, failure := client.Create(context.Background(), input)
	if failure == nil || failure.Uncertain || posts != 0 || strings.Contains(failure.Error(), "секрет из ответа") {
		t.Fatal("ошибка до POST должна быть безопасной для повтора")
	}
	allow = true
	_, failure = client.Create(context.Background(), input)
	if failure != nil || posts != 1 || reporter["accountId"] != "cloud-account" || reporter["name"] != "" {
		t.Fatalf("неверный автор Cloud: %v, %v", reporter, failure)
	}
}
