package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const guestLifetime = 365 * 24 * time.Hour

// ID виден странице для привязки черновиков. Только подписанная cookie
// удостоверяет участника; её значение не передаётся JavaScript.
func signGuest(key [32]byte, id string, expires time.Time) string {
	payload := id + "." + strconv.FormatInt(expires.Unix(), 10)
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(payload))
	return payload + "." + hex.EncodeToString(mac.Sum(nil))
}

func verifyGuest(key [32]byte, value string, now time.Time) string {
	id, rest, ok := strings.Cut(value, ".")
	stamp, _, signed := strings.Cut(rest, ".")
	if !ok || !signed || len(id) < 8 || len(id) > 64 || len(value) > 150 {
		return ""
	}
	expires, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil || expires <= now.Unix() || !hmac.Equal([]byte(value), []byte(signGuest(key, id, time.Unix(expires, 0)))) {
		return ""
	}
	return id
}

func (s *Server) guestID(w http.ResponseWriter, r *http.Request, issue bool) string {
	secure := s.requestScheme(r) == "https" || strings.HasPrefix(s.options.PublicOrigin, "https://")
	name := "retro_session"
	if secure {
		name = "__Host-retro_session"
	}
	now := time.Now()
	if cookies := r.CookiesNamed(name); len(cookies) == 1 {
		if id := verifyGuest(s.options.SessionKey, cookies[0].Value, now); id != "" {
			return id
		}
	}
	if !issue {
		return ""
	}
	var nonce [16]byte
	rand.Read(nonce[:])
	id := hex.EncodeToString(nonce[:])
	expires := now.Add(guestLifetime)
	http.SetCookie(w, &http.Cookie{Name: name, Value: signGuest(s.options.SessionKey, id, expires),
		Path: "/", Expires: expires.UTC(), MaxAge: int(guestLifetime.Seconds()),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	return id
}
