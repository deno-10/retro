package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestForeignBoardLink(t *testing.T) {
	const owner, member, outsider = "owner-00001", "member-00001", "outsider-00001"
	h, st := newTestServerWithStore(t)
	rec, board := do(t, h, "POST", "/api/boards", owner, map[string]string{"title": "Закрытое ретро"})
	requireMutationStatus(t, rec, 201)
	id := board["id"].(string)
	path := "/api/boards/" + id
	column := board["columns"].([]any)[0].(map[string]any)["id"]
	rec, _ = do(t, h, "POST", "/api/boards", outsider, map[string]string{"title": "Другая команда"})
	requireMutationStatus(t, rec, 201)
	admitMembers(t, h, id, owner, member)
	before, _ := st.Get(id)
	admitMembers(t, h, id, owner, member, owner)
	after, _ := st.Get(id)
	if after.Revision != before.Revision {
		t.Fatal("повтор выдачи доступа изменил ревизию")
	}
	rec, _ = do(t, h, "POST", path+"/members", owner, map[string]string{"participantId": " "})
	requireMutationStatus(t, rec, 400)
	rec, _ = do(t, h, "POST", path+"/members", member, map[string]string{"participantId": outsider})
	requireMutationStatus(t, rec, 403)
	rec, _ = do(t, h, "POST", path+"/cards", owner, map[string]any{"columnId": column, "text": "Секретная карточка"})
	requireMutationStatus(t, rec, 200)
	rec, _ = do(t, h, "GET", path+"/export.md", member, nil)
	requireMutationStatus(t, rec, 409)
	rec, _ = do(t, h, "GET", path+"/export.md", outsider, nil)
	requireMutationStatus(t, rec, 404)
	rec, _ = do(t, h, "POST", path+"/writing/join", outsider, map[string]int{"round": 1})
	requireMutationStatus(t, rec, 404)
	rec, _ = do(t, h, "POST", path+"/phase", owner, map[string]string{"phase": "reveal"})
	requireMutationStatus(t, rec, 200)
	before, _ = st.Get(id)
	for _, tc := range []struct {
		method, url string
		body        any
	}{
		{"GET", "/b/" + id, nil},
		{"GET", path, nil}, {"GET", path + "/history", nil}, {"GET", path + "/export.md", nil},
		{"GET", path + "/events", nil}, {"HEAD", path + "/export.md", nil},
		{"POST", path + "/members", map[string]string{"participantId": outsider}},
		{"POST", path + "/cards", map[string]any{"columnId": column, "text": "Попытка записи"}},
		{"POST", path + "/next", map[string]string{"title": "Чужое ретро"}},
		{"POST", path + "/actions/unknown/jira", map[string]int{"revision": 1}},
		{"POST", path + "/actions/unknown/jira/check", map[string]string{}},
	} {
		t.Run(tc.method+" "+tc.url, func(t *testing.T) {
			request := func(url string) *httptest.ResponseRecorder {
				req := mutationRequest(t, tc.method, url, outsider, "", tc.body)
				ctx, cancel := context.WithTimeout(req.Context(), time.Second)
				defer cancel()
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req.WithContext(ctx))
				return w
			}
			denied, missing := request(tc.url), request(strings.Replace(tc.url, id, "unknown-board", 1))
			for _, w := range []*httptest.ResponseRecorder{denied, missing} {
				requireMutationStatus(t, w, http.StatusNotFound)
				if w.Header().Get("Content-Disposition") != "" || strings.Contains(w.Header().Get("Content-Type"), "event-stream") ||
					strings.Contains(w.Body.String(), "Закрытое ретро") || strings.Contains(w.Body.String(), "Секретная карточка") {
					t.Fatal("чужая ссылка раскрыла данные доски")
				}
			}
			if denied.Body.String() != missing.Body.String() {
				t.Fatal("ответы выдают существование закрытой доски")
			}
		})
	}
	after, _ = st.Get(id)
	if after.Revision != before.Revision || after.HasMember(outsider) {
		t.Fatal("посторонний смог изменить доску или её состав")
	}
	for _, actor := range []string{member, owner} {
		for _, url := range []string{"/b/" + id, path, path + "/history", path + "/export.md"} {
			rec, _ = do(t, h, "GET", url, actor, nil)
			requireMutationStatus(t, rec, 200)
			if url != "/b/"+id && !strings.Contains(rec.Body.String(), "Закрытое ретро") {
				t.Fatalf("участнику не вернули доску по %s", url)
			}
			if url == path+"/export.md" && (!strings.Contains(rec.Body.String(), "Секретная карточка") || rec.Header().Get("Content-Disposition") == "") {
				t.Fatal("участнику не вернули экспорт с раскрытыми карточками")
			}
		}
	}
}

func TestHistoryDoesNotOpenAdjacentBoards(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			f.phase("discuss")
			rec, next := do(t, f.handler, "POST", "/api/boards/"+f.id+"/next", mutationOwner, map[string]string{"title": "Второе ретро"})
			requireMutationStatus(t, rec, 201)
			second := next["id"].(string)
			const firstOnly, secondOnly = "first-000001", "second-000001"
			admitMembers(t, f.handler, f.id, mutationOwner, firstOnly)
			admitMembers(t, f.handler, second, mutationOwner, secondOnly)
			f.reopen()
			for _, tc := range []struct{ actor, own, other, hiddenTitle string }{
				{firstOnly, f.id, second, "Второе ретро"},
				{secondOnly, second, f.id, "Повторы и конфликты"},
			} {
				for _, suffix := range []string{"", "/history"} {
					rec, _ := do(t, f.handler, "GET", "/api/boards/"+tc.own+suffix, tc.actor, nil)
					requireMutationStatus(t, rec, 200)
					if strings.Contains(rec.Body.String(), tc.other) || strings.Contains(rec.Body.String(), tc.hiddenTitle) ||
						strings.Contains(rec.Body.String(), `"members"`) {
						t.Fatalf("чужие данные в %s: %s", suffix, rec.Body)
					}
					rec, _ = do(t, f.handler, "GET", "/api/boards/"+tc.other+suffix, tc.actor, nil)
					requireMutationStatus(t, rec, 404)
				}
			}
			// Участник, добавленный до создания следующей доски, видит обе после рестарта.
			rec, _ = do(t, f.handler, "GET", "/api/boards/"+second+"/history", mutationAuthor, nil)
			requireMutationStatus(t, rec, 200)
			var items []historyItem
			if err := json.Unmarshal(rec.Body.Bytes(), &items); err != nil || len(items) != 2 || items[0].ID != f.id || items[1].ID != second {
				t.Fatalf("участник потерял доступную историю: %s", rec.Body)
			}
		})
	}
}
