package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"retro/internal/jira"
	"retro/internal/ollama"
)

// Options — ограничения одного экземпляра. Идентификатор участника и
// присланный клиентом X-Forwarded-For не служат ключами ограничения нагрузки.
type Options struct {
	SessionKey        [32]byte
	AuthMode          string
	BaaSSpace         string
	PublicOrigin      string
	BasePath          string // внешний префикс; шлюз удаляет его перед проксированием
	SharedStore       bool
	Jira              *jira.Client
	Ollama            *ollama.Client
	TrustedProxy      netip.Prefix
	RequestsPerSecond float64
	RequestBurst      int
	CreatesPerMinute  float64
	CreateBurst       int
	SSEPerIP          int
	SSEPerBoard       int
	SSETotal          int
	WriteTimeout      time.Duration
}

func DefaultOptions() Options {
	return Options{RequestsPerSecond: 10, RequestBurst: 200, CreatesPerMinute: 6,
		CreateBurst: 30, SSEPerIP: 20, SSEPerBoard: 50, SSETotal: 200, WriteTimeout: 10 * time.Second}
}

type bucket struct{ tokens float64 }
type clientTraffic struct {
	request, create bucket
	updated         time.Time
	streams         int
}
type trafficGuard struct {
	mu       sync.Mutex
	clients  map[netip.Addr]*clientTraffic
	boards   map[string]int
	streams  int
	inflight chan struct{}
	options  Options
}

func newTrafficGuard(o Options) *trafficGuard {
	return &trafficGuard{clients: make(map[netip.Addr]*clientTraffic), boards: make(map[string]int),
		inflight: make(chan struct{}, 64), options: o}
}

func (g *trafficGuard) clientIP(r *http.Request) netip.Addr {
	ip := remoteIP(r)
	if g.options.TrustedProxy.IsValid() && g.options.TrustedProxy.Contains(ip) {
		if forwarded, err := netip.ParseAddr(r.Header.Get("X-Real-IP")); err == nil {
			return forwarded.Unmap()
		}
	}
	return ip
}

func remoteIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, _ := netip.ParseAddr(host)
	return ip.Unmap()
}

func (s *Server) requestScheme(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if s.options.TrustedProxy.IsValid() && s.options.TrustedProxy.Contains(remoteIP(r)) {
		if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded == "http" || forwarded == "https" {
			scheme = forwarded
		}
	}
	return scheme
}

func (g *trafficGuard) client(ip netip.Addr, now time.Time) *clientTraffic {
	c := g.clients[ip]
	if c == nil {
		// Карту нельзя растить без конца, даже с миллионами разных адресов.
		if len(g.clients) >= 10000 {
			for key, old := range g.clients {
				if old.streams == 0 && now.Sub(old.updated) >= time.Hour {
					delete(g.clients, key)
				}
			}
			if len(g.clients) >= 10000 {
				return nil
			}
		}
		c = &clientTraffic{request: bucket{float64(g.options.RequestBurst)}, create: bucket{float64(g.options.CreateBurst)}, updated: now}
		g.clients[ip] = c
	}
	elapsed := max(0, now.Sub(c.updated).Seconds())
	c.request.tokens = min(float64(g.options.RequestBurst), c.request.tokens+elapsed*g.options.RequestsPerSecond)
	c.create.tokens = min(float64(g.options.CreateBurst), c.create.tokens+elapsed*g.options.CreatesPerMinute/60)
	c.updated = now
	return c
}

func (g *trafficGuard) allow(ip netip.Addr, create bool, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	c := g.client(ip, now)
	if c == nil || c.request.tokens < 1 || (create && c.create.tokens < 1) {
		return false
	}
	c.request.tokens--
	if create {
		c.create.tokens--
	}
	return true
}

// acquireStream делает все три проверки под одной блокировкой. Возврат
// слота идемпотентен и вызывается при отключении, ошибке записи и shutdown.
func (g *trafficGuard) acquireStream(ip netip.Addr, board string) (func(), bool) {
	g.mu.Lock()
	c := g.client(ip, time.Now())
	if c == nil || c.streams >= g.options.SSEPerIP || g.boards[board] >= g.options.SSEPerBoard || g.streams >= g.options.SSETotal {
		g.mu.Unlock()
		return nil, false
	}
	c.streams++
	g.boards[board]++
	g.streams++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			c.streams--
			g.streams--
			g.boards[board]--
			if g.boards[board] == 0 {
				delete(g.boards, board)
			}
		})
	}, true
}

type metrics struct {
	started                     time.Time
	requests, failures, limited atomic.Uint64
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	select {
	case <-s.hub.Done():
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "stopping"})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	select {
	case <-s.hub.Done():
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "stopping"})
		return
	default:
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checker, ok := s.store.(interface{ Ready(context.Context) error })
	if !ok || checker.Ready(ctx) != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	s.traffic.mu.Lock()
	streams := s.traffic.streams
	s.traffic.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprintf(w, "# TYPE retro_http_requests_total counter\nretro_http_requests_total %d\n"+
		"# TYPE retro_http_errors_total counter\nretro_http_errors_total %d\n"+
		"# TYPE retro_http_limited_total counter\nretro_http_limited_total %d\n"+
		"# TYPE retro_sse_connections gauge\nretro_sse_connections %d\n"+
		"# TYPE retro_uptime_seconds gauge\nretro_uptime_seconds %.0f\n"+
		"# TYPE retro_goroutines gauge\nretro_goroutines %d\n",
		s.stats.requests.Load(), s.stats.failures.Load(), s.stats.limited.Load(), streams,
		time.Since(s.stats.started).Seconds(), runtime.NumGoroutine())
}

func rejectBusy(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "10")
	writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "слишком много запросов или соединений; повторите позже"})
}

func (s *Server) protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.options.WriteTimeout))
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "private, no-store")
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				if s.options.AuthMode == "baas" && r.Header.Get("Origin") == "" {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "Origin обязателен"})
					return
				}
				if origin := r.Header.Get("Origin"); origin != "" {
					u, err := url.Parse(origin)
					expected := s.options.PublicOrigin
					if expected == "" {
						expected = s.requestScheme(r) + "://" + r.Host
					}
					if err != nil || origin != expected || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
						writeJSON(w, http.StatusForbidden, map[string]string{"error": "запрос с другого сайта запрещён"})
						return
					}
				}
				if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
					writeJSON(w, http.StatusForbidden, map[string]string{"error": "запрос с другого сайта запрещён"})
					return
				}
			}
			create := r.Method == http.MethodPost && (r.URL.Path == "/api/boards" || strings.HasSuffix(r.URL.Path, "/next"))
			if !s.traffic.allow(s.traffic.clientIP(r), create, time.Now()) {
				rejectBusy(w)
				return
			}
			if !strings.HasSuffix(r.URL.Path, "/events") {
				select {
				case s.traffic.inflight <- struct{}{}:
					defer func() { <-s.traffic.inflight }()
				default:
					rejectBusy(w)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Обёртка сохраняет Flusher и Unwrap, чтобы метрики не ломали SSE/deadline.
type responseLog struct {
	http.ResponseWriter
	status, bytes int
}

func (w *responseLog) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseLog) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseLog) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(data)
	w.bytes += n
	return n, err
}

type flushingLog struct{ *responseLog }

func (w *flushingLog) Flush() {
	_ = w.FlushError()
}
func (w *flushingLog) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		record := &responseLog{ResponseWriter: w}
		var wrapped http.ResponseWriter = record
		if _, ok := w.(http.Flusher); ok {
			wrapped = &flushingLog{record}
		}
		next.ServeHTTP(wrapped, r)
		status := record.status
		if status == 0 {
			status = http.StatusOK
		}
		s.stats.requests.Add(1)
		if status >= 500 {
			s.stats.failures.Add(1)
		}
		if status == 429 {
			s.stats.limited.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/_info" || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			return
		}
		// Не записываем query, Authorization и тела: там участники и тексты ретро.
		s.log.Info("http", "method", r.Method, "route", r.Pattern, "status", status, "bytes", record.bytes, "dur", time.Since(start).Round(time.Millisecond))
	})
}
