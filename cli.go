package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"retro/internal/server"
	"retro/internal/store"
)

// runCLI возвращает ошибки вместо os.Exit: ресурсы закрываются и при сбоях.
func runCLI(ctx context.Context, args []string, out, errOut io.Writer) error {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	f.SetOutput(errOut)
	data := f.String("data", "data/retro.db", "путь к базе относительно текущего каталога")
	var run func() error
	switch command {
	case "serve":
		defaults := server.DefaultOptions()
		addr := f.String("addr", ":8080", "адрес HTTP-сервера")
		proxy := f.String("trusted-proxy", "", "CIDR доверенного прокси; только от него принимается X-Real-IP")
		jsonLog := f.Bool("log-json", false, "структурированные JSON-логи")
		streamsIP := f.Int("sse-per-ip", defaults.SSEPerIP, "число SSE-соединений с одного IP, включая общий NAT команды")
		streamsBoard := f.Int("sse-per-board", defaults.SSEPerBoard, "число SSE-соединений одной доски")
		streamsTotal := f.Int("sse-total", defaults.SSETotal, "общее число SSE-соединений")
		run = func() error {
			opts := server.DefaultOptions()
			if *streamsIP <= 0 || *streamsBoard <= 0 || *streamsTotal <= 0 {
				return errors.New("лимиты SSE должны быть положительными")
			}
			opts.SSEPerIP, opts.SSEPerBoard, opts.SSETotal = *streamsIP, *streamsBoard, *streamsTotal
			if *proxy != "" {
				p, err := netip.ParsePrefix(*proxy)
				if err != nil {
					return fmt.Errorf("неверный trusted-proxy: %w", err)
				}
				opts.TrustedProxy = p
			}
			var handler slog.Handler = slog.NewTextHandler(out, nil)
			if *jsonLog {
				handler = slog.NewJSONHandler(out, nil)
			}
			return serve(ctx, *addr, *data, opts, slog.New(handler))
		}
	case "healthcheck":
		url := f.String("url", "http://127.0.0.1:8080/readyz", "адрес проверки готовности")
		run = func() error {
			client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, *url, nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("сервис не готов: HTTP %d", resp.StatusCode)
			}
			return nil
		}
	case "backup":
		dest := f.String("out", "", "новый файл снимка; - для двоичного вывода в stdout")
		run = func() error {
			if *dest == "" {
				return errors.New("укажите -out для резервной копии")
			}
			if *dest != "-" {
				return store.BackupSQLite(ctx, *data, *dest)
			}
			// Снимок может быть больше tmpfs контейнера; используем диск БД.
			dir, err := os.MkdirTemp(filepath.Dir(*data), ".retro-backup-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			path := filepath.Join(dir, "retro.db")
			if err := store.BackupSQLite(ctx, *data, path); err != nil {
				return err
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			_, err = io.Copy(out, file)
			return err
		}
	case "verify":
		run = func() error { return store.VerifySQLite(ctx, *data) }
	case "restore":
		source := f.String("from", "", "проверенный SQLite-снимок для восстановления")
		run = func() error {
			if *source == "" {
				return errors.New("укажите -from для восстановления в новую базу")
			}
			return store.BackupSQLite(ctx, *source, *data)
		}
	case "transfer-owner":
		board := f.String("board", "", "ID доски")
		participant := f.String("participant", "", "новый ID участника из браузера ведущего")
		run = func() error { return store.TransferOwner(ctx, *data, *board, *participant) }
	default:
		return fmt.Errorf("неизвестная команда %q; доступны serve, healthcheck, backup, verify, restore, transfer-owner", command)
	}
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("лишние аргументы: %v", f.Args())
	}
	return run()
}
