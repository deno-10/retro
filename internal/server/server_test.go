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

func TestNextBoardAndHistory(t *testing.T) {
	h := newTestServer(t)
	const owner, alice = "owner-00000001", "alice-00000001"

	_, out := do(t, h, "POST", "/api/boards", owner, map[string]any{"title": "Спринт 1", "template": "4l"})
	first := out["id"].(string)
	do(t, h, "POST", "/api/boards/"+first+"/phase", owner, map[string]any{"phase": "discuss"})
	do(t, h, "POST", "/api/boards/"+first+"/actions", alice, map[string]any{"text": "Открытое", "assignee": "Боб"})
	_, out = do(t, h, "POST", "/api/boards/"+first+"/actions", alice, map[string]any{"text": "Закрытое"})
	acts := out["actions"].([]any)
	closedID := acts[len(acts)-1].(map[string]any)["id"].(string)
	do(t, h, "POST", "/api/boards/"+first+"/actions/"+closedID+"/done", alice, nil)

	if rec, _ := do(t, h, "POST", "/api/boards/"+first+"/next", alice, map[string]any{"title": "Спринт 2"}); rec.Code != http.StatusForbidden {
		t.Fatalf("следующее ретро не ведущим: %d", rec.Code)
	}
	rec, out := do(t, h, "POST", "/api/boards/"+first+"/next", owner, map[string]any{"title": "Спринт 2"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("следующее ретро: %d %s", rec.Code, rec.Body)
	}
	second := out["id"].(string)
	if out["prevId"] != first || out["title"] != "Спринт 2" || out["phase"] != "write" {
		t.Fatalf("новая доска: %v", out)
	}
	acts = out["actions"].([]any)
	if len(acts) != 1 || acts[0].(map[string]any)["carried"] != true || acts[0].(map[string]any)["text"] != "Открытое" {
		t.Fatalf("должно перенестись одно открытое решение с пометкой: %v", acts)
	}
	_, out = do(t, h, "GET", "/api/boards/"+first, alice, nil)
	if out["nextId"] != second {
		t.Fatalf("предыдущая доска должна ссылаться на следующую: %v", out["nextId"])
	}
	if rec, _ := do(t, h, "POST", "/api/boards/"+first+"/next", owner, map[string]any{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("второе следующее ретро: %d", rec.Code)
	}

	req := httptest.NewRequest("GET", "/api/boards/"+second+"/history", nil)
	req.Header.Set("X-Participant", alice)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var chain []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &chain); err != nil || w.Code != http.StatusOK {
		t.Fatalf("история: %d %s", w.Code, w.Body)
	}
	if len(chain) != 2 || chain[0]["id"] != first || chain[1]["id"] != second {
		t.Fatalf("цепочка от первого к последнему: %v", chain)
	}
	if chain[0]["current"] != false || chain[1]["current"] != true || chain[0]["openActions"].(float64) != 1 {
		t.Fatalf("пометка текущего и счётчик открытых решений: %v", chain)
	}
}

func TestGroupEndpoints(t *testing.T) {
	h := newTestServer(t)
	const owner, alice = "owner-00000001", "alice-00000001"

	_, out := do(t, h, "POST", "/api/boards", owner, map[string]any{"title": "Группы", "template": "classic"})
	id := out["id"].(string)
	colID := out["columns"].([]any)[0].(map[string]any)["id"].(string)
	var ids []string
	for _, text := range []string{"первая", "вторая"} {
		_, out = do(t, h, "POST", "/api/boards/"+id+"/cards", alice, map[string]any{"columnId": colID, "text": text})
		cards := out["columns"].([]any)[0].(map[string]any)["cards"].([]any)
		ids = append(ids, cards[len(cards)-1].(map[string]any)["id"].(string))
	}
	if rec, _ := do(t, h, "POST", "/api/boards/"+id+"/cards/"+ids[0]+"/group", owner, map[string]any{"targetCardId": ids[1]}); rec.Code != http.StatusConflict {
		t.Fatalf("группировка в фазе записи: %d", rec.Code)
	}
	do(t, h, "POST", "/api/boards/"+id+"/phase", owner, map[string]any{"phase": "reveal"})
	if rec, _ := do(t, h, "POST", "/api/boards/"+id+"/cards/"+ids[0]+"/group", alice, map[string]any{"targetCardId": ids[1]}); rec.Code != http.StatusForbidden {
		t.Fatalf("группировка не ведущим: %d", rec.Code)
	}
	rec, out := do(t, h, "POST", "/api/boards/"+id+"/cards/"+ids[0]+"/group", owner, map[string]any{"targetCardId": ids[1]})
	if rec.Code != http.StatusOK {
		t.Fatalf("группировка: %d %s", rec.Code, rec.Body)
	}
	cards := out["columns"].([]any)[0].(map[string]any)["cards"].([]any)
	g0, g1 := cards[0].(map[string]any)["groupId"], cards[1].(map[string]any)["groupId"]
	if g0 == nil || g0 != g1 {
		t.Fatalf("обе карточки должны быть в одной группе: %v", cards)
	}
	rec, out = do(t, h, "DELETE", "/api/boards/"+id+"/cards/"+ids[0]+"/group", owner, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("разгруппировка: %d %s", rec.Code, rec.Body)
	}
	for _, c := range out["columns"].([]any)[0].(map[string]any)["cards"].([]any) {
		if c.(map[string]any)["groupId"] != nil {
			t.Fatalf("после выхода одной из двух карточек группа должна распасться: %v", c)
		}
	}
}

func TestActionsFlow(t *testing.T) {
	h := newTestServer(t)
	const owner, alice, bob = "owner-00000001", "alice-00000001", "bob-0000000001"

	_, out := do(t, h, "POST", "/api/boards", owner, map[string]any{"title": "Решения", "template": "classic"})
	id := out["id"].(string)

	rec, _ := do(t, h, "POST", "/api/boards/"+id+"/actions", alice, map[string]any{"text": "рано"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("решение в фазе записи: %d", rec.Code)
	}
	if rec, _ = do(t, h, "POST", "/api/boards/"+id+"/phase", owner, map[string]any{"phase": "discuss"}); rec.Code != http.StatusOK {
		t.Fatalf("смена фазы: %d", rec.Code)
	}
	rec, out = do(t, h, "POST", "/api/boards/"+id+"/actions", alice,
		map[string]any{"text": "Настроить алерты", "assignee": "Боб"})
	if rec.Code != http.StatusOK {
		t.Fatalf("добавление решения: %d %s", rec.Code, rec.Body)
	}
	actions := out["actions"].([]any)
	if len(actions) != 1 {
		t.Fatalf("ожидалось одно решение: %v", out["actions"])
	}
	action := actions[0].(map[string]any)
	actionID := action["id"].(string)
	if action["assignee"] != "Боб" || action["mine"] != true || action["done"] != false {
		t.Fatalf("неожиданное решение: %v", action)
	}

	if rec, _ = do(t, h, "PATCH", "/api/boards/"+id+"/actions/"+actionID, bob, map[string]any{"text": "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("правка чужого решения: %d", rec.Code)
	}
	rec, out = do(t, h, "POST", "/api/boards/"+id+"/actions/"+actionID+"/done", owner, nil)
	if rec.Code != http.StatusOK || out["actions"].([]any)[0].(map[string]any)["done"] != true {
		t.Fatalf("отметка сделанным ведущим: %d %s", rec.Code, rec.Body)
	}
	rec, _ = do(t, h, "GET", "/api/boards/"+id+"/export.md", bob, nil)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("- [x] Настроить алерты — Боб")) {
		t.Fatalf("экспорт решений: %d %s", rec.Code, rec.Body)
	}
	if rec, _ = do(t, h, "DELETE", "/api/boards/"+id+"/actions/"+actionID, bob, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("удаление чужого решения: %d", rec.Code)
	}
	rec, out = do(t, h, "DELETE", "/api/boards/"+id+"/actions/"+actionID, alice, nil)
	if rec.Code != http.StatusOK || len(out["actions"].([]any)) != 0 {
		t.Fatalf("удаление своего решения: %d %s", rec.Code, rec.Body)
	}
	if rec, _ = do(t, h, "DELETE", "/api/boards/"+id+"/actions/nope", owner, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("удаление несуществующего решения: %d", rec.Code)
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
