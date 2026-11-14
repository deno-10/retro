package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"retro/internal/testutil"
	"strings"
	"testing"
	"time"
)

func TestPaaSRuntime(t *testing.T) {
	if os.Getenv("RETRO_PAAS_PROBE") == "1" {
		os.Args = []string{"retro"}
		main()
		os.Exit(0)
	}
	dsn := testutil.PostgresURL(t)
	t.Setenv("RETRO_DATABASE_URL", dsn)
	if err := runCLI(context.Background(), []string{"migrate"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	origin := "http://" + addr
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPaaSRuntime$")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "RETRO_") && !strings.HasPrefix(v, "JIRA_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "RETRO_PAAS_PROBE=1", "RETRO_PROFILE=paas", "RETRO_DATABASE_URL="+dsn, "RETRO_ADDR="+addr, "RETRO_BAAS_SPACE=retro-test", "RETRO_TRUSTED_PROXY=127.0.0.1/32", "RETRO_PUBLIC_ORIGIN="+origin)
	logPath := filepath.Join(t.TempDir(), "server.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(os.Interrupt)
		if err := cmd.Wait(); err != nil {
			data, _ := os.ReadFile(logPath)
			t.Errorf("остановка PaaS-процесса: %v %s", err, data)
		}
	})
	client := http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for {
		res, err := client.Get(origin + "/readyz")
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("процесс PaaS не готов")
		}
		time.Sleep(30 * time.Millisecond)
	}
	for path, want := range map[string]int{"/_info": 200, "/healthz": 200, "/api/templates": 401, "/": 401} {
		res, err := client.Get(origin + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("%s: %d", path, res.StatusCode)
		}
	}
	req, _ := http.NewRequest("POST", origin+"/api/boards", strings.NewReader(`{"title":"PaaS из окружения"}`))
	req.Header.Set("Origin", origin)
	req.Header.Set("X-Baas-User-Id", "00000000-1111-2222-3333-444444444444")
	req.Header.Set("X-Baas-Space-Name", "retro-test")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 201 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("корпоративное создание: %d %s", res.StatusCode, body)
	}
}

func TestPaaSRejectsSQLiteMaintenance(t *testing.T) {
	t.Setenv("RETRO_PROFILE", "paas")
	dest := filepath.Join(t.TempDir(), "backup.db")
	for _, args := range [][]string{{"backup", "-out", dest}, {"verify"}, {"restore", "-from", dest}, {"transfer-owner", "-board", "some", "-participant", "owner-1234"}} {
		if err := runCLI(context.Background(), args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "SQLite") {
			t.Fatal(fmt.Sprintf("команда принята в PaaS: %v %v", args, err))
		}
	}
}
