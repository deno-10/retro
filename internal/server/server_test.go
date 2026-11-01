package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"testing/fstest"

	"retro/internal/hub"
	"retro/internal/store"
)

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	st, err := store.OpenJSON(filepath.Join(t.TempDir(), "boards.json"))
	if err != nil {
		t.Fatal(err)
	}
	static := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}}
	return New(st, hub.New(), static, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func do(t *testing.T, h http.Handler, method, path, participant string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Participant", participant)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func firstCard(t *testing.T, view map[string]any) map[string]any {
	t.Helper()
	cols := view["columns"].([]any)
	cards := cols[0].(map[string]any)["cards"].([]any)
	if len(cards) == 0 {
		t.Fatalf("в первой колонке нет карточек: %v", view)
	}
	return cards[0].(map[string]any)
}

func TestBoardFlow(t *testing.T) {
	h := newTestServer(t)
	const owner, alice, bob = "owner-00000001", "alice-00000001", "bob-0000000001"

	rec, out := do(t, h, "POST", "/api/boards", owner, map[string]any{"title": "Спринт 7", "template": "ssc"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("создание доски: %d %s", rec.Code, rec.Body)
	}
	id := out["id"].(string)
	colID := out["columns"].([]any)[0].(map[string]any)["id"].(string)
	if out["isOwner"] != true {
		t.Fatal("создатель должен быть ведущим")
	}

	rec, _ = do(t, h, "POST", "/api/boards/"+id+"/cards", alice,
		map[string]any{"columnId": colID, "text": "деплой по пятницам", "author": "Алиса"})
	if rec.Code != http.StatusOK {
		t.Fatalf("добавление карточки: %d %s", rec.Code, rec.Body)
	}

	_, out = do(t, h, "GET", "/api/boards/"+id, bob, nil)
	if card := firstCard(t, out); card["hidden"] != true || card["text"] != "" {
		t.Fatalf("карточка Алисы должна быть скрыта от Боба: %v", card)
	}

	rec, _ = do(t, h, "POST", "/api/boards/"+id+"/phase", bob, map[string]any{"phase": "reveal"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("смена фазы не ведущим: %d", rec.Code)
	}
	rec, _ = do(t, h, "POST", "/api/boards/"+id+"/phase", owner, map[string]any{"phase": "reveal"})
	if rec.Code != http.StatusOK {
		t.Fatalf("смена фазы ведущим: %d %s", rec.Code, rec.Body)
	}

	_, out = do(t, h, "GET", "/api/boards/"+id, bob, nil)
	if card := firstCard(t, out); card["text"] != "деплой по пятницам" || card["author"] != "Алиса" {
		t.Fatalf("после показа карточка должна быть видна: %v", card)
	}

	rec, _ = do(t, h, "GET", "/api/boards/"+id+"/export.md", bob, nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("деплой по пятницам")) {
		t.Fatalf("экспорт: %d %s", rec.Code, rec.Body)
	}

	rec, _ = do(t, h, "GET", "/api/boards/nope", bob, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("несуществующая доска: %d", rec.Code)
	}
	rec, _ = do(t, h, "GET", "/api/boards/"+id, "x", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("короткий идентификатор участника: %d", rec.Code)
	}
}

func TestTimerEndpoint(t *testing.T) {
	h := newTestServer(t)
	const owner, bob = "owner-00000001", "bob-0000000001"

	_, out := do(t, h, "POST", "/api/boards", owner, map[string]any{"title": "Таймер", "template": "classic"})
	id := out["id"].(string)

	rec, _ := do(t, h, "POST", "/api/boards/"+id+"/timer", bob, map[string]any{"minutes": 5})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("таймер не ведущим: %d", rec.Code)
	}
	rec, _ = do(t, h, "POST", "/api/boards/"+id+"/timer", owner, map[string]any{"minutes": 500})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("слишком долгий таймер: %d", rec.Code)
	}
	rec, out = do(t, h, "POST", "/api/boards/"+id+"/timer", owner, map[string]any{"minutes": 5})
	if rec.Code != http.StatusOK || out["timerEndsAt"] == nil {
		t.Fatalf("запуск таймера: %d %s", rec.Code, rec.Body)
	}
	_, out = do(t, h, "GET", "/api/boards/"+id, bob, nil)
	if out["timerEndsAt"] == nil {
		t.Fatal("участник должен видеть таймер")
	}
	rec, out = do(t, h, "POST", "/api/boards/"+id+"/timer", owner, map[string]any{"minutes": 0})
	if rec.Code != http.StatusOK || out["timerEndsAt"] != nil {
		t.Fatalf("остановка таймера: %d %s", rec.Code, rec.Body)
	}
}
