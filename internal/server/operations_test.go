package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"retro/internal/hub"
	"retro/internal/model"
	"retro/internal/store"
)

func TestReadinessReflectsStoreAndShutdown(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "retro.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	handler := New(st, h, fstest.MapFS{"index.html": {Data: []byte("ok")}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	check := func(path string, status int) {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != status {
			t.Fatalf("%s: %d вместо %d", path, w.Code, status)
		}
	}
	check("/readyz", 200)
	st.Close()
	check("/readyz", 503)
	check("/healthz", 200)
	h.Close()
	check("/healthz", 503)
	check("/readyz", 503)
	check("/metrics", 200)
}

func TestTrafficDoesNotTrustClientHeaders(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		opts := testOptions()
		opts.RequestBurst, opts.RequestsPerSecond = 1, 0
		if trusted {
			opts.TrustedProxy = netip.MustParsePrefix("192.0.2.0/24")
		}
		g := newTrafficGuard(opts)
		r := httptest.NewRequest("GET", "/api/templates", nil)
		r.RemoteAddr = "192.0.2.1:1234"
		r.Header.Set("X-Real-IP", "198.51.100.1")
		if !g.allow(g.clientIP(r), false, time.Now()) {
			t.Fatal("первый запрос отклонён")
		}
		r.Header.Set("X-Real-IP", "198.51.100.2")
		r.Header.Set("X-Forwarded-For", "203.0.113.1")
		r.Header.Set("X-Participant", "another-identity")
		if got := g.allow(g.clientIP(r), false, time.Now()); got != trusted {
			t.Fatalf("доверенный прокси=%v, пропущено=%v", trusted, got)
		}
	}
}

func TestRateLimitRefillAndCreationBudget(t *testing.T) {
	o := testOptions()
	o.RequestBurst, o.RequestsPerSecond = 3, 1
	o.CreateBurst, o.CreatesPerMinute = 1, 1
	g := newTrafficGuard(o)
	ip := netip.MustParseAddr("127.0.0.1")
	now := time.Now()
	if !g.allow(ip, true, now) || g.allow(ip, true, now) || !g.allow(ip, false, now) {
		t.Fatal("квота создания не отделена от чтений")
	}
	if g.allow(ip, true, now.Add(59*time.Second)) {
		t.Fatal("квота создания восстановилась слишком рано")
	}
	if !g.allow(ip, true, now.Add(time.Minute)) {
		t.Fatal("квота не восстановилась")
	}
}

func TestStreamQuotasAreAtomicAndReleased(t *testing.T) {
	o := testOptions()
	o.SSEPerIP, o.SSEPerBoard, o.SSETotal = 1, 2, 3
	g := newTrafficGuard(o)
	ip := netip.MustParseAddr("127.0.0.1")
	var accepted atomic.Int32
	var wg sync.WaitGroup
	releases := make(chan func(), 10)
	for i := 0; i < 10; i++ {
		wg.Go(func() {
			if release, ok := g.acquireStream(ip, "board"); ok {
				accepted.Add(1)
				releases <- release
			}
		})
	}
	wg.Wait()
	close(releases)
	if accepted.Load() != 1 {
		t.Fatalf("вместо одного слота занято %d", accepted.Load())
	}
	for release := range releases {
		release()
		release()
	}
	if g.streams != 0 || len(g.boards) != 0 {
		t.Fatal("слот не освобождён или освобождён дважды")
	}
	a, _ := g.acquireStream(ip, "board")
	defer a()
	b, _ := g.acquireStream(netip.MustParseAddr("127.0.0.2"), "board")
	defer b()
	if _, ok := g.acquireStream(netip.MustParseAddr("127.0.0.3"), "board"); ok {
		t.Fatal("лимит доски превышен")
	}
	c, _ := g.acquireStream(netip.MustParseAddr("127.0.0.3"), "another")
	defer c()
	if _, ok := g.acquireStream(netip.MustParseAddr("127.0.0.4"), "third"); ok {
		t.Fatal("общий лимит превышен")
	}
}

func TestHTTPRateLimitAndCrossSiteRequests(t *testing.T) {
	st, _ := store.OpenJSON(filepath.Join(t.TempDir(), "retro.json"))
	o := testOptions()
	o.RequestBurst, o.RequestsPerSecond = 1, 0
	h := New(st, hub.New(), fstest.MapFS{"index.html": {Data: []byte("ok")}}, slog.New(slog.NewTextHandler(io.Discard, nil)), o)
	request := func(method, path, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(`{"title":"Ретро"}`))
		r.Header.Set("Origin", origin)
		testGuest(r, "owner-00000001")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if got := request("POST", "/api/boards", "https://evil.example"); got.Code != 403 {
		t.Fatalf("запрос с другого сайта: %d", got.Code)
	}
	if got := request("POST", "/api/boards", "null"); got.Code != 403 {
		t.Fatalf("непрозрачный Origin: %d", got.Code)
	}
	if got := request("GET", "/api/templates", ""); got.Code != 200 {
		t.Fatal(got.Code)
	}
	if got := request("POST", "/api/boards", ""); got.Code != 429 || got.Header().Get("Retry-After") == "" {
		t.Fatalf("нет корректного 429: %d", got.Code)
	}
	if got := request("GET", "/readyz", ""); got.Code != 200 {
		t.Fatal("проверка готовности заблокирована лимитом")
	}
	if got := request("GET", "/metrics", ""); !strings.Contains(got.Body.String(), "retro_http_limited_total 1") {
		t.Fatal("отказ не отражён в метриках")
	}
}

type failingFlush struct {
	header    http.Header
	deadlines []time.Time
}

func (w *failingFlush) Header() http.Header         { return w.header }
func (w *failingFlush) Write(p []byte) (int, error) { return len(p), nil }
func (w *failingFlush) WriteHeader(int)             {}
func (w *failingFlush) Flush()                      {}
func (w *failingFlush) FlushError() error {
	return errors.New("клиент перестал читать")
}
func (w *failingFlush) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func TestSSEFlushFailureReleasesCapacityAndPreservesDeadline(t *testing.T) {
	f := newMutationFixture(t, "db")
	w := &failingFlush{header: make(http.Header)}
	r := httptest.NewRequest("GET", "/api/boards/"+f.id+"/events", nil)
	testGuest(r, mutationOwner)
	f.handler.ServeHTTP(w, r)
	if len(w.deadlines) < 2 || w.deadlines[len(w.deadlines)-1].IsZero() {
		t.Fatal("SSE не поставил дедлайн записи через обёртку логов")
	}
	metrics := httptest.NewRecorder()
	f.handler.ServeHTTP(metrics, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(metrics.Body.String(), "retro_sse_connections 0") || f.hub.Count(f.id) != 0 {
		t.Fatal("неудачная запись оставила подписку/занятый слот")
	}
}

func TestReceiptCapacityStillAllowsOldRetry(t *testing.T) {
	f := newMutationFixture(t, "db")
	body := f.payload("cards", "Исходная карточка")
	path := "/api/boards/" + f.id + "/cards"
	rec, _ := doMutation(t, f.handler, "POST", path, mutationAuthor, "original-key", body)
	requireMutationStatus(t, rec, http.StatusOK)
	_, err := f.store.Update(f.id, func(b *model.Board) error {
		for len(b.Receipts) < model.MaxReceipts {
			b.Receipts[fmt.Sprint(len(b.Receipts))] = model.MutationReceipt{}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := f.state()
	rec, _ = doMutation(t, f.handler, "POST", path, mutationAuthor, "new-key-0001", f.payload("cards", "Не поместится"))
	requireMutationStatus(t, rec, http.StatusConflict)
	rec, _ = doMutation(t, f.handler, "POST", path, mutationAuthor, "original-key", body)
	requireMutationStatus(t, rec, http.StatusOK)
	after := f.state()
	if before.Revision != after.Revision || len(after.Columns[0].Cards) != 1 {
		t.Fatal("отказ/повтор изменил данные")
	}
}

func TestReadyHonorsCanceledContext(t *testing.T) {
	st, _ := store.OpenSQLite(filepath.Join(t.TempDir(), "retro.db"))
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := st.Ready(ctx); err == nil {
		t.Fatal("отмена проверки проигнорирована")
	}
}

func TestSSESurvivesIdleLongerThanWriteDeadline(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "retro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b, _ := model.NewBoard("Долгий поток", "classic", "owner-00000001")
	if err := st.Create(b); err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	o := testOptions()
	o.WriteTimeout = 25 * time.Millisecond
	handler := New(st, h, fstest.MapFS{"index.html": {Data: []byte("ok")}}, slog.New(slog.NewTextHandler(io.Discard, nil)), o)
	ts := httptest.NewServer(handler)
	defer ts.Close()
	defer h.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest("GET", ts.URL + "/api/boards/" + b.ID + "/events", nil)
	testGuest(req, b.OwnerID)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	next := func() string {
		t.Helper()
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("SSE преждевременно закрылся: %v", err)
			}
			if strings.HasPrefix(line, "data: ") {
				return line
			}
		}
	}
	next()
	time.Sleep(3 * o.WriteTimeout)
	if _, err := st.Update(b.ID, func(b *model.Board) error { b.Title = "После паузы"; return nil }); err != nil {
		t.Fatal(err)
	}
	h.Notify(b.ID)
	if line := next(); !strings.Contains(line, "После паузы") {
		t.Fatalf("не пришло обновление после простоя: %s", line)
	}
}
