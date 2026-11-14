package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"retro/internal/hub"
	"retro/internal/model"
	"retro/internal/store"
	"retro/internal/testutil"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func clusterNode(t *testing.T, dsn string) (http.Handler, *store.PostgresStore) {
	t.Helper()
	st, err := store.OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := hub.New()
	t.Cleanup(func() { h.Close(); st.Close() })
	o := testOptions()
	o.SharedStore = true
	return New(st, h, fstest.MapFS{"index.html": {Data: []byte("<html><head></head></html>")}}, slog.New(slog.NewTextHandler(io.Discard, nil)), o), st
}

func TestClusterSSEAndWriting(t *testing.T) {
	dsn := testutil.PostgresURL(t)
	a, st := clusterNode(t, dsn)
	b, _ := clusterNode(t, dsn)
	const owner = "owner-1234"
	board, _ := model.NewBoard("Общее ретро", "ssc", owner)
	for i := 1; i < 15; i++ {
		actor := fmt.Sprintf("actor-%04d", i)
		if _, err := board.AddMember(actor, owner); err != nil {
			t.Fatal(err)
		}
		if _, err := board.JoinWriting(actor, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Create(board); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/boards/"+board.ID+"/events", nil)
	testGuest(req, owner)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	updates := make(chan model.BoardView, 20)
	go func() {
		defer close(updates)
		scanner := bufio.NewScanner(res.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				var v model.BoardView
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v) == nil {
					select {
					case updates <- v:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	select {
	case <-updates:
	case <-ctx.Done():
		t.Fatal("нет начального SSE")
	}
	// Только второй узел получает мутации: первый узнаёт о них из общей БД.
	var wg sync.WaitGroup
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pid := owner
			if i > 0 {
				pid = fmt.Sprintf("actor-%04d", i)
			}
			w, _ := do(t, b, "POST", "/api/boards/"+board.ID+"/writing/ready", pid, map[string]any{"round": 1, "revision": 0, "ready": true})
			if w.Code != 200 {
				t.Errorf("готовность: %d %s", w.Code, w.Body)
			}
		}(i)
	}
	wg.Wait()
	for {
		select {
		case v, ok := <-updates:
			if !ok {
				t.Fatal("SSE закрылся до обновления")
			}
			if v.Phase == model.PhaseReveal {
				goto received
			}
		case <-ctx.Done():
			t.Fatal("изменение второго узла не пришло через SSE")
		}
	}
received:
	saved, _ := st.Get(board.ID)
	if saved.Revision != board.Revision+15 || saved.Phase != model.PhaseReveal {
		t.Fatal("готовность потеряна или этап переключён дважды")
	}
	w, _ := do(t, a, "POST", "/api/boards/"+board.ID+"/writing/ready", owner, map[string]any{"round": 1, "revision": 0, "ready": true})
	if w.Code != 200 {
		t.Fatal("повтор готовности на другом узле не идемпотентен")
	}
	after, _ := st.Get(board.ID)
	if after.Revision != saved.Revision {
		t.Fatal("повтор изменил ревизию")
	}
}

func TestClusterRecoversTimerWithoutBrowsers(t *testing.T) {
	dsn := testutil.PostgresURL(t)
	st, err := store.OpenPostgres(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	board, _ := model.NewBoard("Таймер после рестарта", "ssc", "owner-1234")
	past := time.Now().Add(-time.Minute)
	board.TimerEndsAt = &past
	if err = st.Create(board); err != nil {
		t.Fatal(err)
	}
	st.Close()
	_, first := clusterNode(t, dsn)
	_, second := clusterNode(t, dsn)
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := first.Get(board.ID)
		if err != nil {
			t.Fatal(err)
		}
		if b.Phase == model.PhaseReveal {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("таймер не восстановлен без HTTP-запроса")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(1100 * time.Millisecond)
	b, err := second.Get(board.ID)
	if err != nil || b.Phase != model.PhaseReveal || b.TimerEndsAt != nil || b.Revision != board.Revision+1 {
		t.Fatalf("таймер применился не ровно один раз: %+v %v", b, err)
	}
}
