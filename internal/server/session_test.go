package server

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

var testSessionKey = func() (key [32]byte) { rand.Read(key[:]); return }()

func testOptions() Options {
	o := DefaultOptions()
	o.SessionKey = testSessionKey
	return o
}

func testGuest(r *http.Request, id string) {
	r.AddCookie(&http.Cookie{Name: "retro_session", Value: signGuest(testSessionKey, id, time.Now().Add(time.Hour))})
}

func TestGuestSessionBoundary(t *testing.T) {
	h := newTestServer(t)
	request := func(method, path, claim string, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if claim != "" {
			r.Header.Set("X-Participant", claim)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	open := func(page string) *http.Cookie {
		w := request("GET", page, "", nil, "")
		cookies := w.Result().Cookies()
		if w.Code != 200 || len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode || cookies[0].Path != "/" || cookies[0].Domain != "" || cookies[0].MaxAge <= 0 {
			t.Fatal("страница должна создавать изолированную долговременную сессию")
		}
		if strings.Contains(w.Body.String(), cookies[0].Value) || !strings.Contains(w.Header().Get("Cache-Control"), "private") {
			t.Fatal("страница или кеш раскрывают cookie")
		}
		return cookies[0]
	}
	owner := open("/")
	other := open("/")
	https := open("https://retro.example/")
	if !https.Secure || https.Name != "__Host-retro_session" || owner.Value == other.Value {
		t.Fatal("HTTPS и независимые клиенты должны получать защищённые cookie")
	}
	ownerID := verifyGuest(testSessionKey, owner.Value, time.Now())
	w := request("POST", "/api/boards", ownerID, owner, `{"title":"Доска ведущего"}`)
	var board struct {
		ID      string
		IsOwner bool
		Columns []struct{ ID string }
	}
	if err := json.Unmarshal(w.Body.Bytes(), &board); err != nil || w.Code != 201 || !board.IsOwner {
		t.Fatalf("не удалось создать доску с сессией, HTTP %d", w.Code)
	}
	path := "/api/boards/" + board.ID
	if len(board.Columns) == 0 {
		t.Fatal("не удалось прочитать колонки тестовой доски")
	}
	raw, _ := json.Marshal(map[string]string{"columnId": board.Columns[0].ID, "text": "Черновик ведущего"})
	created := request("POST", path+"/cards", ownerID, owner, string(raw))
	if created.Code != 200 {
		t.Fatalf("не удалось добавить карточку, HTTP %d", created.Code)
	}
	if w := request("GET", path, "", other, ""); w.Code != 404 {
		t.Fatalf("чужая сессия открыла доску по ссылке, HTTP %d", w.Code)
	}
	admitMembers(t, h, board.ID, ownerID, verifyGuest(testSessionKey, other.Value, time.Now()))
	foreign := request("GET", path, "", other, "")
	if foreign.Code != 200 || strings.Contains(foreign.Body.String(), "Черновик ведущего") {
		t.Fatal("участник увидел чужой черновик")
	}
	if request("GET", path, ownerID, other, "").Code != 409 {
		t.Fatal("подмена заголовка раскрывает чужие карточки")
	}
	for _, tc := range []struct {
		name, query, claim string
		cookie             *http.Cookie
		status             int
	}{
		{"только заголовок", "", ownerID, nil, 401},
		{"только URL", "?participant=" + ownerID, "", nil, 401},
		{"чужая сессия", "", ownerID, other, 409},
		{"старый URL", "?participant=" + ownerID, "", other, 409},
		{"чужой участник", "", "", other, 403},
		{"ID вместо cookie", "", "", &http.Cookie{Name: owner.Name, Value: ownerID}, 401},
		{"изменённая подпись", "", "", &http.Cookie{Name: owner.Name, Value: owner.Value + "0"}, 401},
		{"истёкшая подпись", "", "", &http.Cookie{Name: owner.Name, Value: signGuest(testSessionKey, ownerID, time.Now().Add(-time.Minute))}, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := request("POST", path+"/phase"+tc.query, tc.claim, tc.cookie, `{"phase":"reveal"}`).Code; got != tc.status {
				t.Fatalf("HTTP %d, ожидали %d", got, tc.status)
			}
		})
	}
	if request("GET", path+"/events?participant="+ownerID, "", nil, "").Code != 401 ||
		request("GET", path+"/events?participant="+ownerID, "", other, "").Code != 409 {
		t.Fatal("поток принял старый идентификатор из URL")
	}
	if got := request("POST", path+"/phase", ownerID, owner, `{"phase":"reveal"}`).Code; got != 200 {
		t.Fatalf("ведущий потерял доступ, HTTP %d", got)
	}
}
