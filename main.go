// Retro — доска для ретроспектив спринта. Один бинарник: API, SSE и статика.
//
//	go run .                       # http://localhost:8080, данные в data/retro.db
//	go run . -addr :9000 -data /var/lib/retro/retro.db
//	go run . -data data/boards.json  # прежнее JSON-хранилище
package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"retro/internal/hub"
	"retro/internal/server"
	"retro/internal/store"
)

//go:embed web
var webFS embed.FS

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, cfg runtimeConfig, logger *slog.Logger) error {
	// Сначала занимаем порт: ошибка запуска возвращается вызывающему коду,
	// до открытия/миграции данных и до сообщения об успешном запуске.
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("не удалось открыть порт: %w", err)
	}
	defer listener.Close()
	var st store.Store
	if cfg.Storage == "postgres" {
		st, err = store.OpenPostgres(cfg.DatabaseURL)
	} else {
		st, err = openStore(cfg.Data, logger)
	}
	if err != nil {
		return fmt.Errorf("не удалось открыть хранилище: %w", err)
	}
	defer st.Close()
	if cfg.Storage == "postgres" {
		readyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = st.(*store.PostgresStore).Ready(readyCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("схема PostgreSQL не готова; выполните migrate: %w", err)
		}
	}
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	h := hub.New()
	defer h.Close()
	options := cfg.Options
	if options.AuthMode != "baas" {
		options.SessionKey, err = loadSessionKey(filepath.Join(filepath.Dir(cfg.Data), "retro-session.key"))
		if err != nil {
			return fmt.Errorf("не удалось открыть ключ сессий: %w", err)
		}
	}
	options.SharedStore = cfg.Storage == "postgres"
	srv := &http.Server{
		Handler:           server.New(st, h, static, logger, options),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		// Общий WriteTimeout оборвал бы долгоживущие SSE. HTTP-слой
		// ставит отдельный дедлайн на каждый ответ/запись в поток.
	}
	result := make(chan error, 1)
	logger.Info("retro запущен", "addr", listener.Addr().String(), "storage", cfg.Storage)
	if cfg.Options.PublicOrigin != "" {
		logger.Info("ссылка для команды", "url", cfg.Options.PublicOrigin+cfg.Options.BasePath+"/")
	} else {
		for _, u := range shareURLs(listener.Addr().String(), lanIPs()) {
			logger.Info("ссылка для команды", "url", u)
		}
	}
	go func() { result <- srv.Serve(listener) }()

	select {
	case err := <-result:
		srv.Close()
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("сервер остановлен с ошибкой: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	h.Close() // закрыть SSE-потоки, иначе Shutdown будет ждать их до таймаута
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		srv.Close()
		return fmt.Errorf("остановка с ошибкой: %w", err)
	}
	logger.Info("retro остановлен")
	return nil
}

// shareURLs — адреса, по которым команда откроет доску. Если сервер слушает
// все интерфейсы (":8080" или "0.0.0.0:8080"), подставляются адреса машины
// в локальной сети, чтобы ссылку можно было сразу скинуть коллегам.
func shareURLs(addr string, ips []net.IP) []string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	if host != "" && host != "0.0.0.0" && host != "::" {
		return []string{"http://" + net.JoinHostPort(host, port)}
	}
	urls := []string{"http://localhost:" + port}
	for _, ip := range ips {
		urls = append(urls, "http://"+net.JoinHostPort(ip.String(), port))
	}
	return urls
}

// lanIPs — IPv4-адреса машины на поднятых интерфейсах, кроме loopback.
func lanIPs() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var ips []net.IP
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipn.IP.To4(); ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() {
				ips = append(ips, ip4)
			}
		}
	}
	return ips
}

// openStore открывает хранилище по пути. Для SQLite при пустой базе один раз
// импортирует доски из boards.json, лежащего в том же каталоге, — так старые
// установки переезжают на новый формат без ручных действий. Импорт не
// повторится: после него база уже не пуста.
func openStore(path string, logger *slog.Logger) (store.Store, error) {
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	sq, ok := st.(*store.SQLiteStore)
	if !ok {
		return st, nil
	}
	empty, err := sq.Empty()
	if err != nil {
		sq.Close()
		return nil, err
	}
	if !empty {
		return st, nil
	}
	legacy := filepath.Join(filepath.Dir(path), "boards.json")
	n, err := store.ImportJSON(sq, legacy)
	if err != nil {
		sq.Close()
		return nil, err
	}
	if n > 0 {
		logger.Info("доски импортированы из JSON", "from", legacy, "count", n)
	}
	return st, nil
}
