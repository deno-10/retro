package server

import (
	"net/http"
	"testing"

	"retro/internal/model"
)

func TestPatchRevisionRejectsStaleEditsWithoutOverwriting(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		for _, kind := range []string{"cards", "actions"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				collection := "/api/boards/" + f.id + "/" + kind
				rec, view := do(t, f.handler, "POST", collection, mutationAuthor, f.payload(kind, "Исходный текст"))
				requireMutationStatus(t, rec, http.StatusOK)
				initial := mutationItems(view, kind)[0].(map[string]any)
				path := collection + "/" + initial["id"].(string)
				rec, _ = do(t, f.handler, "PATCH", path, mutationOwner,
					map[string]any{"text": "Правка ведущего", "assignee": "Борис", "revision": initial["revision"]})
				requireMutationStatus(t, rec, http.StatusOK)
				beforeConflict := f.state()
				rec, _ = do(t, f.handler, "PATCH", path, mutationAuthor,
					map[string]any{"text": "Устаревшая правка автора", "assignee": "Вера", "revision": initial["revision"]})
				requireMutationStatus(t, rec, http.StatusConflict)
				if after := f.state(); after.Revision != beforeConflict.Revision || !after.UpdatedAt.Equal(beforeConflict.UpdatedAt) {
					t.Fatal("конфликт не должен менять доску")
				}
				_, view = do(t, f.handler, "GET", "/api/boards/"+f.id, mutationAuthor, nil)
				latest := mutationItems(view, kind)[0].(map[string]any)
				if latest["text"] != "Правка ведущего" || latest["revision"].(float64) != initial["revision"].(float64)+1 {
					t.Fatalf("свежая правка потеряна: %v", latest)
				}
				if kind == "actions" && latest["assignee"] != "Борис" {
					t.Fatal("устаревший запрос перезаписал ответственного")
				}
			})
		}
	}
}

func TestPatchRequiresRevisionAndAcceptsLegacyZero(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		for _, kind := range []string{"cards", "actions"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				collection := "/api/boards/" + f.id + "/" + kind
				rec, view := do(t, f.handler, "POST", collection, mutationAuthor, f.payload(kind, "Старая запись"))
				requireMutationStatus(t, rec, http.StatusOK)
				item := mutationItems(view, kind)[0].(map[string]any)
				path := collection + "/" + item["id"].(string)
				for _, body := range []map[string]any{
					{"text": "Без версии"},
					{"text": "Пустая версия", "revision": nil},
					{"text": "Отрицательная версия", "revision": -1},
					{"text": "Дробная версия", "revision": 1.5},
					{"text": "Строковая версия", "revision": "1"},
				} {
					rec, _ := do(t, f.handler, "PATCH", path, mutationAuthor, body)
					requireMutationStatus(t, rec, http.StatusBadRequest)
				}
				if _, err := f.store.Update(f.id, func(b *model.Board) error {
					if kind == "cards" {
						b.Columns[0].Cards[0].Revision = 0
					} else {
						b.Actions[0].Revision = 0
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				f.reopen()
				rec, view = do(t, f.handler, "PATCH", path, mutationAuthor,
					map[string]any{"text": "Обновлено из legacy", "assignee": "Борис", "revision": 0})
				requireMutationStatus(t, rec, http.StatusOK)
				latest := mutationItems(view, kind)[0].(map[string]any)
				if latest["text"] != "Обновлено из legacy" || latest["revision"].(float64) != 1 {
					t.Fatalf("нулевая версия старой записи должна приниматься: %v", latest)
				}
			})
		}
	}
}

func TestActionDoneRequiresExplicitIdempotentState(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			collection := "/api/boards/" + f.id + "/actions"
			rec, view := do(t, f.handler, "POST", collection, mutationAuthor, f.payload("actions", "Задача"))
			requireMutationStatus(t, rec, http.StatusOK)
			initial := mutationItems(view, "actions")[0].(map[string]any)
			path := collection + "/" + initial["id"].(string)
			for _, body := range []any{nil, map[string]any{}, map[string]any{"done": nil}, map[string]any{"done": "true"}} {
				rec, _ := do(t, f.handler, "POST", path+"/done", mutationAuthor, body)
				requireMutationStatus(t, rec, http.StatusBadRequest)
			}
			for _, done := range []bool{true, false} {
				rec, view = do(t, f.handler, "POST", path+"/done", mutationOwner, map[string]any{"done": done})
				requireMutationStatus(t, rec, http.StatusOK)
				first := mutationItems(view, "actions")[0].(map[string]any)
				rec, view = do(t, f.handler, "POST", path+"/done", mutationAuthor, map[string]any{"done": done})
				requireMutationStatus(t, rec, http.StatusOK)
				repeated := mutationItems(view, "actions")[0].(map[string]any)
				if repeated["done"] != done || repeated["revision"] != first["revision"] {
					t.Fatalf("повтор должен сохранить состояние и версию решения: %v", repeated)
				}
			}
			rec, _ = do(t, f.handler, "POST", path+"/done", "stranger-00001", map[string]any{"done": true})
			requireMutationStatus(t, rec, http.StatusForbidden)
			rec, _ = do(t, f.handler, "PATCH", path, mutationAuthor,
				map[string]any{"text": "Редактор открыт до отметки", "revision": initial["revision"]})
			requireMutationStatus(t, rec, http.StatusConflict)
			if action := f.state().Actions[0]; action.Text != "Задача" || action.Done {
				t.Fatalf("устаревшая правка изменила решение: %+v", action)
			}
		})
	}
}
