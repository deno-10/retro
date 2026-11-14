package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"retro/internal/model"
)

func rawJSONRequest(h http.Handler, method, path, body, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	testGuest(req, mutationOwner)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func requireRequestPreservedBoard(t *testing.T, f *mutationFixture, before *model.Board, signals <-chan struct{}) {
	t.Helper()
	if after := f.state(); !reflect.DeepEqual(after, before) {
		t.Fatalf("отклонённое тело запроса изменило доску: до=%+v, после=%+v", before, after)
	}
	select {
	case <-signals:
		t.Fatal("отклонённое тело запроса оповестило подписчиков")
	default:
	}
}

func TestTimerRequiredMinutesPreservesBoard(t *testing.T) {
	invalid := []struct{ name, body string }{
		{"missing", `{}`},
		{"null-field", `{"minutes":null}`},
		{"null-body", `null`},
		{"unknown-field", `{"seconds":60}`},
		{"string", `{"minutes":"1"}`},
		{"boolean", `{"minutes":true}`},
		{"fraction", `{"minutes":1.5}`},
		{"array-field", `{"minutes":[]}`},
		{"object-field", `{"minutes":{}}`},
		{"array-body", `[]`},
		{"overflow", `{"minutes":99999999999999999999999999}`},
		{"malformed", `{"minutes":0`},
		{"second-value", `{"minutes":0} null`},
	}
	for _, backend := range []string{"json", "db"} {
		for _, expired := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/expired=%t", backend, expired), func(t *testing.T) {
				f := newMutationFixture(t, backend)
				path := "/api/boards/" + f.id + "/timer"
				if expired {
					setTimerEnd(t, f.store, f.id, time.Now().Add(-time.Minute))
					f.reopen() // Нет работающего планировщика: дедлайн ждёт первого корректного обращения.
				} else {
					rec := rawJSONRequest(f.handler, "POST", path, `{"minutes":5}`, "")
					requireMutationStatus(t, rec, http.StatusOK)
				}
				before := f.state()
				signals, unsubscribe := f.hub.Subscribe(f.id)
				defer unsubscribe()
				for _, tc := range invalid {
					t.Run(tc.name, func(t *testing.T) {
						rec := rawJSONRequest(f.handler, "POST", path, tc.body, "")
						requireMutationStatus(t, rec, http.StatusBadRequest)
						requireRequestPreservedBoard(t, f, before, signals)
					})
				}
				if expired {
					rec, _ := do(t, f.handler, "GET", "/api/boards/"+f.id, mutationOwner, nil)
					requireMutationStatus(t, rec, http.StatusOK)
					after := f.state()
					if after.Phase != model.PhaseVote || after.TimerEndsAt != nil || after.Revision != before.Revision+1 {
						t.Fatal("корректное чтение после ошибок запроса не применило истечение таймера ровно один раз")
					}
					requireExpiryNotification(t, signals)
				} else {
					rec := rawJSONRequest(f.handler, "POST", path, `{"minutes":0}`, "")
					requireMutationStatus(t, rec, http.StatusOK)
					if f.state().TimerEndsAt != nil {
						t.Fatal("явный minutes=0 должен останавливать таймер")
					}
				}
			})
		}
	}
}

func TestTimerMinutesRangeAndWhitespace(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			path := "/api/boards/" + f.id + "/timer"
			for _, minutes := range []int{1, model.MaxTimerMinutes} {
				start := time.Now()
				body := fmt.Sprintf(" \t\r\n{\"minutes\":%d,\"unknown\":true}\r\n\t ", minutes)
				rec := rawJSONRequest(f.handler, "POST", path, body, "")
				requireMutationStatus(t, rec, http.StatusOK)
				finish := time.Now()
				deadline := f.state().TimerEndsAt
				duration := time.Duration(minutes) * time.Minute
				if deadline == nil || deadline.Before(start.Add(duration)) || deadline.After(finish.Add(duration)) {
					t.Fatalf("таймер не запущен на %d минут: %v", minutes, deadline)
				}
			}
			before := f.state()
			signals, unsubscribe := f.hub.Subscribe(f.id)
			defer unsubscribe()
			for _, minutes := range []int{-1, model.MaxTimerMinutes + 1} {
				rec := rawJSONRequest(f.handler, "POST", path, fmt.Sprintf(`{"minutes":%d}`, minutes), "")
				requireMutationStatus(t, rec, http.StatusBadRequest)
				requireRequestPreservedBoard(t, f, before, signals)
			}
			rec := rawJSONRequest(f.handler, "POST", path, "{\"minutes\":0}\n\t ", "")
			requireMutationStatus(t, rec, http.StatusOK)
			if f.state().TimerEndsAt != nil {
				t.Fatal("minutes=0 с пробельным остатком не остановил таймер")
			}
		})
	}
}

func TestTrailingJSONRejectedBeforeCreateAndReceipt(t *testing.T) {
	suffixes := []struct{ name, body string }{
		{"object", ` {}`},
		{"array", ` []`},
		{"null", ` null`},
		{"boolean", ` true`},
		{"number", ` 42`},
		{"string", ` "ещё"`},
		{"garbage", ` trailing-garbage`},
		{"incomplete-object", ` {`},
	}
	for _, backend := range []string{"json", "db"} {
		for _, kind := range []string{"cards", "actions"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				f := newMutationFixture(t, backend)
				path := "/api/boards/" + f.id + "/" + kind
				body, err := json.Marshal(f.payload(kind, "Не должно сохраниться"))
				if err != nil {
					t.Fatal(err)
				}
				before := f.state()
				signals, unsubscribe := f.hub.Subscribe(f.id)
				defer unsubscribe()
				for _, suffix := range suffixes {
					t.Run(suffix.name, func(t *testing.T) {
						rec := rawJSONRequest(f.handler, "POST", path, string(body)+suffix.body, "invalid-body-01")
						requireMutationStatus(t, rec, http.StatusBadRequest)
						requireRequestPreservedBoard(t, f, before, signals)
					})
				}
				for _, malformed := range []string{"", " \r\n\t", `{"text":`, `{"text":"обрыв}`, `{"text":"x",}`} {
					rec := rawJSONRequest(f.handler, "POST", path, malformed, "invalid-body-01")
					requireMutationStatus(t, rec, http.StatusBadRequest)
					requireRequestPreservedBoard(t, f, before, signals)
				}
				// Отвергнутые тела не занимают ключ: исправленный запрос выполняется один раз.
				rec := rawJSONRequest(f.handler, "POST", path, string(body)+" \r\n\t", "invalid-body-01")
				requireMutationStatus(t, rec, http.StatusOK)
				after := f.state()
				if len(after.Receipts) != 1 || after.Revision != before.Revision+1 {
					t.Fatal("отклонённое тело оставило квитанцию или частичную мутацию")
				}
			})
		}
	}
}

func TestTrailingJSONRejectedBeforeEditingAndNextBoard(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			cardID := addVoteCard(t, f, "Сохранённый текст")
			before := f.state()
			signals, unsubscribe := f.hub.Subscribe(f.id)
			defer unsubscribe()
			path := "/api/boards/" + f.id
			for _, request := range []struct{ method, path, body string }{
				{"PATCH", path + "/cards/" + cardID, fmt.Sprintf(`{"text":"Другой текст","revision":%d} null`, before.Columns[0].Cards[0].Revision)},
				{"POST", path + "/phase", `{"phase":"vote"} {}`},
				{"POST", path + "/next", `{"title":"Следующая доска"} true`},
			} {
				rec := rawJSONRequest(f.handler, request.method, request.path, request.body, "")
				requireMutationStatus(t, rec, http.StatusBadRequest)
				requireRequestPreservedBoard(t, f, before, signals)
			}
		})
	}
}

func TestJSONRequestLimitIncludesTrailingWhitespace(t *testing.T) {
	const limit = 1 << 20
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			path := "/api/boards/" + f.id + "/actions"
			body := `{"text":"Решение"}`
			before := f.state()
			signals, unsubscribe := f.hub.Subscribe(f.id)
			defer unsubscribe()
			for _, oversized := range []string{
				body + strings.Repeat(" ", limit+1-len(body)),
				body + strings.Repeat("\n", limit) + "null",
				`{"text":"Решение","unknown":"` + strings.Repeat("x", limit) + `"}`,
			} {
				rec := rawJSONRequest(f.handler, "POST", path, oversized, "size-limit-0001")
				requireMutationStatus(t, rec, http.StatusBadRequest)
				requireRequestPreservedBoard(t, f, before, signals)
			}
			exact := body + strings.Repeat(" ", limit-len(body))
			rec := rawJSONRequest(f.handler, "POST", path, exact, "size-limit-0001")
			requireMutationStatus(t, rec, http.StatusOK)
			after := f.state()
			if len(after.Actions) != 1 || len(after.Receipts) != 1 || after.Revision != before.Revision+1 {
				t.Fatal("тело ровно в 1 МиБ должно сохранять одну запись и одну квитанцию")
			}
		})
	}
}

func TestSingleJSONPreservesDefaultCreateAndUnknownFields(t *testing.T) {
	for _, backend := range []string{"json", "db"} {
		t.Run(backend, func(t *testing.T) {
			f := newMutationFixture(t, backend)
			for _, body := range []string{"null \r\n\t", "{} \n", `{"unknown":"совместимость"}`} {
				rec := rawJSONRequest(f.handler, "POST", "/api/boards", body, "")
				requireMutationStatus(t, rec, http.StatusCreated)
				var view model.BoardView
				if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
					t.Fatal(err)
				}
				if view.ID == "" || len(view.Columns) == 0 {
					t.Fatal("создание с параметрами по умолчанию потеряло совместимость")
				}
			}
		})
	}
}
