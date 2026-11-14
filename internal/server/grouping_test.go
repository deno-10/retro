package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"retro/internal/model"
	"retro/internal/ollama"
)

func TestGroupSuggestionAccessAndConfirmation(t *testing.T) {
	h, st := newTestServerWithStore(t)
	const owner, member, outsider = "owner-00000001", "member-0000001", "outside-000001"
	b, err := model.NewBoard("Предложения", "classic", owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddMember(member, owner); err != nil {
		t.Fatal(err)
	}
	col := b.Columns[0].ID
	first, err := b.AddCard(col, "Долгие встречи", "", member)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.AddCard(col, "Долгие созвоны", "", member)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Create(b); err != nil {
		t.Fatal(err)
	}
	path := "/api/boards/" + b.ID
	payload := map[string]any{
		"revision": b.Revision, "columns": []model.ColumnGroups{{ColumnID: col, Groups: [][]string{{first.ID, second.ID}}}},
	}
	check := func(actor string, status int) {
		t.Helper()
		for _, endpoint := range []struct {
			method, suffix string
			body           any
		}{
			{"GET", "/group-suggestions", nil}, {"POST", "/groups", payload},
		} {
			rec, _ := do(t, h, endpoint.method, path+endpoint.suffix, actor, endpoint.body)
			requireMutationStatus(t, rec, status)
			if strings.Contains(rec.Body.String(), first.ID) || strings.Contains(rec.Body.String(), "встречи") {
				t.Fatalf("в недоступном ответе есть карточка: %s", rec.Body.String())
			}
		}
	}
	check(owner, http.StatusConflict)
	check(outsider, http.StatusNotFound)
	rec, view := do(t, h, "POST", path+"/phase", owner, map[string]string{"phase": "reveal"})
	requireMutationStatus(t, rec, http.StatusOK)
	check(member, http.StatusForbidden)
	check(outsider, http.StatusNotFound)
	rec, proposed := do(t, h, "GET", path+"/group-suggestions", owner, nil)
	requireMutationStatus(t, rec, http.StatusOK)
	if view["revision"] != proposed["revision"] || !strings.Contains(rec.Body.String(), first.ID) || strings.Contains(rec.Body.String(), "созвоны") {
		t.Fatalf("план должен содержать ID и версию доски: %s", rec.Body.String())
	}
	payload["revision"] = proposed["revision"]
	rec, view = do(t, h, "PATCH", path+"/cards/"+first.ID, member, map[string]any{"text": "Долгие встречи по пятницам", "revision": first.Revision})
	requireMutationStatus(t, rec, http.StatusOK)
	rec, _ = do(t, h, "POST", path+"/groups", owner, payload)
	requireMutationStatus(t, rec, http.StatusConflict)
	rec, unchanged := do(t, h, "GET", path, owner, nil)
	requireMutationStatus(t, rec, http.StatusOK)
	if unchanged["revision"] != view["revision"] || firstCard(t, unchanged)["groupId"] != nil {
		t.Fatal("устаревший план изменил доску")
	}
	rec, _ = do(t, h, "POST", path+"/groups", owner, map[string]any{"columns": payload["columns"]})
	requireMutationStatus(t, rec, http.StatusBadRequest)
	payload["revision"] = unchanged["revision"]
	rec, grouped := do(t, h, "POST", path+"/groups", owner, payload)
	requireMutationStatus(t, rec, http.StatusOK)
	group := firstCard(t, grouped)["groupId"]
	if group == nil {
		t.Fatal("подтверждение не создало группу")
	}
	rec, _ = do(t, h, "POST", path+"/groups", owner, payload)
	requireMutationStatus(t, rec, http.StatusConflict)
	rec, shared := do(t, h, "GET", path, member, nil)
	requireMutationStatus(t, rec, http.StatusOK)
	if firstCard(t, shared)["groupId"] != group || shared["revision"] != grouped["revision"] {
		t.Fatal("повтор изменил состав или версию")
	}
	rec, _ = do(t, h, "POST", path+"/phase", owner, map[string]string{"phase": "vote"})
	requireMutationStatus(t, rec, http.StatusOK)
	check(owner, http.StatusConflict)
}

func TestQwenDraftPrivacyPermissionsAndStaleReply(t *testing.T) {
	const owner, member, outsider = "owner-00000001", "member-0000001", "outside-000001"
	var calls atomic.Int32
	var beforeReply func()
	modelHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/api/chat" {
			t.Errorf("неверный путь к модели: %s %s", r.Method, r.URL)
		}
		for _, private := range []string{owner, member, "Автор карточки", "Закрытое ретро", "Уже в группе", "Одна в колонке"} {
			if strings.Contains(string(raw), private) {
				t.Errorf("лишние данные в запросе к модели: %s", private)
			}
		}
		var req struct {
			Model         string
			Messages      []struct{ Role, Content string }
			Stream, Think bool
		}
		if json.Unmarshal(raw, &req) != nil || req.Model != "qwen3:4b" || len(req.Messages) != 2 || req.Messages[1].Role != "user" || req.Stream || req.Think {
			t.Error("неверный запрос к локальной модели")
			return
		}
		var cards []map[string]any
		if json.Unmarshal([]byte(req.Messages[1].Content), &cards) != nil {
			t.Error("ожидали список карточек")
			return
		}
		a, b := "Пауза, пока ждём ревью", "Долго никто не смотрит PR"
		if n == 2 {
			a, b = "Работает сборка", "Успешный билд"
		}
		want := []map[string]any{{"id": float64(1), "text": a}, {"id": float64(2), "text": b}}
		if !reflect.DeepEqual(cards, want) {
			t.Errorf("в запросе должны быть карточки только одной колонки с временными номерами: %v", cards)
		}
		if n == 3 {
			beforeReply()
		}
		json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": `{"groups":[[2,1]]}`}, "done": true})
	}))
	defer modelHTTP.Close()
	opts := testOptions()
	client, err := ollama.New("qwen3:4b", strings.TrimPrefix(modelHTTP.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	opts.Ollama = client
	h, st := newTestServerWithStoreOptions(t, opts)
	board, err := model.NewBoard("Закрытое ретро", "classic", owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := board.AddMember(member, owner); err != nil {
		t.Fatal(err)
	}
	add := func(col int, text string) string {
		c, err := board.AddCard(board.Columns[col].ID, text, "Автор карточки", member)
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	a, b := add(0, "Пауза, пока ждём ревью"), add(0, "Долго никто не смотрит PR")
	g1, g2 := add(0, "Уже в группе 1"), add(0, "Уже в группе 2")
	c, d := add(1, "Работает сборка"), add(1, "Успешный билд")
	add(2, "Одна в колонке")
	if err := st.Create(board); err != nil {
		t.Fatal(err)
	}
	path := "/api/boards/" + board.ID
	call := func(actor string, revision uint64, status int) map[string]any {
		t.Helper()
		rec, result := do(t, h, "POST", path+"/group-suggestions/qwen", actor, map[string]any{"revision": revision})
		requireMutationStatus(t, rec, status)
		return result
	}
	call(owner, board.Revision, 409)
	if _, err := st.Update(board.ID, func(b *model.Board) error {
		if err := b.SetPhase(model.PhaseReveal, owner); err != nil {
			return err
		}
		return b.GroupCards(g1, g2, owner)
	}); err != nil {
		t.Fatal(err)
	}
	baseline, err := st.Get(board.ID)
	if err != nil {
		t.Fatal(err)
	}
	rev := baseline.Revision
	call(member, rev, 403)
	call(outsider, rev, 404)
	call(owner, rev-1, 409)
	rec, rules := do(t, h, "GET", path+"/group-suggestions", owner, nil)
	requireMutationStatus(t, rec, 200)
	if calls.Load() != 0 || rules["model"] != "qwen3:4b" || rules["source"] != "rules" {
		t.Fatal("словарь не должен запускать модель")
	}
	res := call(owner, rev, 200)
	raw, _ := json.Marshal(res["columns"])
	var columns []model.ColumnGroups
	if err := json.Unmarshal(raw, &columns); err != nil {
		t.Fatal(err)
	}
	want := []model.ColumnGroups{{ColumnID: board.Columns[0].ID, Groups: [][]string{{b, a}}}, {ColumnID: board.Columns[1].ID, Groups: [][]string{{d, c}}}}
	if res["source"] != "qwen" || !reflect.DeepEqual(columns, want) || calls.Load() != 2 {
		t.Fatalf("план модели: %v", res)
	}
	after, err := st.Get(board.ID)
	if err != nil || !reflect.DeepEqual(baseline, after) {
		t.Fatal("предпросмотр изменил доску")
	}
	beforeReply = func() {
		if _, err := st.Update(board.ID, func(b *model.Board) error { return b.SetPhase(model.PhaseVote, owner) }); err != nil {
			t.Error(err)
		}
	}
	res = call(owner, rev, 409)
	if res["columns"] != nil || calls.Load() != 3 {
		t.Fatal("обработка доски продолжилась после смены этапа")
	}
}
