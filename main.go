// Retro — доска для ретроспектив спринта. Один бинарник: API, SSE и статика.
//
//	go run .                       # http://localhost:8080
//	go run . -addr :9000 -data /var/lib/retro/boards.json
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"retro/internal/hub"
	"retro/internal/server"
	"retro/internal/store"
)

//go:embed web
var webFS embed.FS

func main() {
	addr := flag.String("addr", ":8080", "адрес, на котором слушать")
	dataFile := flag.String("data", "data/boards.json", "файл с данными досок")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	st, err := store.OpenJSON(*dataFile)
	if err != nil {
		logger.Error("не удалось открыть хранилище", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	static, err := fs.Sub(webFS, "web")
	if err != nil {
		logger.Error("статика не найдена в бинарнике", "err", err)
		os.Exit(1)
	}

	h := hub.New()
	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.New(st, h, static, logger),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("retro запущен", "addr", *addr, "data", *dataFile)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("сервер остановлен с ошибкой", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	h.Close() // закрыть SSE-потоки, иначе Shutdown будет ждать их до таймаута
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("остановка с ошибкой", "err", err)
	}
	logger.Info("retro остановлен")
}
