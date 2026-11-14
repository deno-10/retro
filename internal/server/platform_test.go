package server

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"retro/internal/hub"
	"retro/internal/store"
)

func TestBaaSIdentityBoundary(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "retro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := hub.New()
	defer h.Close()
	o := DefaultOptions()
	o.AuthMode = "baas"
	o.BaaSSpace = "retro-test"
	o.TrustedProxy = netip.MustParsePrefix("127.0.0.1/32")
	o.PublicOrigin = "https://retro.example.test"
	o.BasePath = "/team/retro"
	app := New(st, h, fstest.MapFS{"index.html": {Data: []byte(`<html><head><script src="/static/app.js"></script></head></html>`)}}, slog.New(slog.NewTextHandler(io.Discard, nil)), o)
	const user = "00000000-1111-2222-3333-444444444444"
	id := fmt.Sprintf("%x", sha256.Sum256([]byte(o.BaaSSpace+"\x00"+user)))
	request := func(method, path, body string, change func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("X-Baas-User-Id", user)
		r.Header.Set("X-Baas-Space-Name", o.BaaSSpace)
		r.Header.Set("Origin", o.PublicOrigin)
		if change != nil {
			change(r)
		}
		w := httptest.NewRecorder()
		app.ServeHTTP(w, r)
		return w
	}
	for name, change := range map[string]func(*http.Request){
		"нет пользователя": func(r *http.Request) { r.Header.Del("X-Baas-User-Id") },
		"чужой space":      func(r *http.Request) { r.Header.Set("X-Baas-Space-Name", "another") },
		"прямой запрос":    func(r *http.Request) { r.RemoteAddr = "192.0.2.4:12345" },
		"не UUID":          func(r *http.Request) { r.Header.Set("X-Baas-User-Id", "owner-123456") },
		"дублирование":     func(r *http.Request) { r.Header.Add("X-Baas-User-Id", user) },
	} {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{"/", "/api/templates", "/api/boards/unknown/events", "/api/boards/unknown/export.md"} {
				if w := request("GET", path, "", change); w.Code != 401 {
					t.Fatalf("%s: %d", path, w.Code)
				}
			}
		})
	}
	for _, path := range []string{"/_info", "/healthz", "/readyz"} {
		if w := request("GET", path, "", func(r *http.Request) { r.Header = http.Header{} }); w.Code != 200 {
			t.Fatalf("probe: %s %d", path, w.Code)
		}
	}
	w := request("GET", "/", "", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), id) || !strings.Contains(w.Body.String(), `src="/team/retro/static/app.js"`) || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("персональный HTML: %d %s", w.Code, w.Body)
	}
	if w := request("POST", "/api/boards", `{"title":"BaaS"}`, func(r *http.Request) { r.Header.Set("X-Participant", "forged-owner") }); w.Code != 409 {
		t.Fatalf("подмена участника принята: %d", w.Code)
	}
	if w := request("GET", "/api/boards/unknown/events?participant=forged-owner", "", nil); w.Code != 409 {
		t.Fatal("подмена в SSE принята")
	}
	for _, origin := range []string{"", "https://attacker.test"} {
		if w := request("POST", "/api/boards", `{"title":"BaaS"}`, func(r *http.Request) { r.Header.Set("Origin", origin) }); w.Code != 403 {
			t.Fatalf("CSRF: %d", w.Code)
		}
	}
	w = request("POST", "/api/boards", `{"title":"BaaS"}`, func(r *http.Request) { r.Header.Set("X-Participant", id) })
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"isOwner":true`) {
		t.Fatalf("создание от имени BaaS: %d %s", w.Code, w.Body)
	}
}
