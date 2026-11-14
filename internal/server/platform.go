package server

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type identityKey struct{}

var baasUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var basePathPattern = regexp.MustCompile(`^(/[A-Za-z0-9_-]+)*$`)

// ValidatePlatform вызывается до открытия порта/БД. Неполная настройка
// корпоративного входа никогда не переключает сервис в анонимный режим.
func (o Options) ValidatePlatform() error {
	if o.AuthMode != "" && o.AuthMode != "local" && o.AuthMode != "baas" {
		return fmt.Errorf("RETRO_AUTH_MODE: допустимы local и baas")
	}
	if !basePathPattern.MatchString(o.BasePath) {
		return fmt.Errorf("RETRO_BASE_PATH: ожидается путь вроде /retro без завершающего слеша")
	}
	if o.AuthMode == "baas" {
		if strings.TrimSpace(o.BaaSSpace) == "" || !o.TrustedProxy.IsValid() || o.TrustedProxy.Bits() == 0 || o.PublicOrigin == "" {
			return fmt.Errorf("для BaaS обязательны RETRO_BAAS_SPACE, узкий RETRO_TRUSTED_PROXY и RETRO_PUBLIC_ORIGIN")
		}
	}
	if o.PublicOrigin != "" {
		u, err := url.Parse(o.PublicOrigin)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("RETRO_PUBLIC_ORIGIN: ожидается http(s) origin без пути")
		}
		if u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
			return fmt.Errorf("RETRO_PUBLIC_ORIGIN: HTTP разрешён только на localhost")
		}
	}
	return nil
}

// BaaS удостоверяет личность через доверенный шлюз. Локальные сессии
// удостоверяются подписанной cookie, общей для вкладок одного браузера.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_info", "/healthz", "/readyz", "/metrics":
			next.ServeHTTP(w, r)
			return
		}
		var id string
		if s.options.AuthMode == "baas" {
			user, space := r.Header.Get("X-Baas-User-Id"), r.Header.Get("X-Baas-Space-Name")
			if !s.options.TrustedProxy.Contains(remoteIP(r)) || len(r.Header.Values("X-Baas-User-Id")) != 1 || len(r.Header.Values("X-Baas-Space-Name")) != 1 || !baasUUID.MatchString(user) || space != s.options.BaaSSpace {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "откройте Retro через корпоративный вход"})
				return
			}
			id = fmt.Sprintf("%x", sha256.Sum256([]byte(space+"\x00"+strings.ToLower(user))))
		} else {
			if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/api/templates" {
				next.ServeHTTP(w, r)
				return
			}
			issue := r.Method == http.MethodGet && (r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/b/"))
			id = s.guestID(w, r, issue)
			if id == "" {
				w.Header().Set("Cache-Control", "private, no-store")
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "откройте Retro заново и разрешите cookie для этого сайта"})
				return
			}
		}
		w.Header().Set("Cache-Control", "private, no-store")
		// Заголовок связывает изменения со вкладкой при смене сессии. В URL
		// участника не принимаем даже со старой версии клиента.
		claims := r.Header.Values("X-Participant")
		if r.URL.Query().Has("participant") || len(claims) > 1 || (len(claims) == 1 && claims[0] != id) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "участник изменился, обновите страницу"})
			return
		}
		authenticated := r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
		next.ServeHTTP(w, authenticated)
		r.Pattern = authenticated.Pattern
	})
}

// Любой экземпляр может применить истёкший таймер; Store.Update обеспечивает
// единственный переход. Проверка работает и после полного рестарта кластера.
func (s *Server) recoverTimers() {
	source, ok := s.store.(interface {
		DueTimers(context.Context, time.Time) ([]string, error)
	})
	if !ok {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.hub.Done():
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		ids, err := source.DueTimers(ctx, time.Now())
		cancel()
		if err != nil {
			s.log.Error("не удалось восстановить таймеры", "err", err)
		}
		for _, id := range ids {
			select {
			case <-s.hub.Done():
				return
			default:
			}
			s.mu.Lock()
			_, err := s.syncTimerLocked(id)
			s.mu.Unlock()
			if err != nil {
				s.log.Error("таймер не восстановлен", "board", id, "err", err)
			}
		}
		select {
		case <-s.hub.Done():
			return
		case <-ticker.C:
		}
	}
}
