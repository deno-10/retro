package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestStartupFailureExitsNonzero(t *testing.T) {
	if os.Getenv("RETRO_STARTUP_PROBE") == "1" {
		os.Args = []string{"retro", "-addr", os.Getenv("RETRO_PROBE_ADDR"), "-data", os.Getenv("RETRO_PROBE_DATA")}
		main()
		os.Exit(0)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	data := filepath.Join(t.TempDir(), "should-not-exist.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartupFailureExitsNonzero$")
	cmd.Env = append(os.Environ(), "RETRO_STARTUP_PROBE=1", "RETRO_PROBE_ADDR="+listener.Addr().String(), "RETRO_PROBE_DATA="+data)
	output, err := cmd.CombinedOutput()
	if err == nil || cmd.ProcessState.ExitCode() != 1 {
		t.Fatalf("ошибка запуска не дала exit 1: %v; %s", err, output)
	}
	if strings.Contains(string(output), "retro запущен") {
		t.Fatal("сообщение о запуске появилось до bind")
	}
	if _, err := os.Stat(data); !os.IsNotExist(err) {
		t.Fatal("ошибка порта создала базу")
	}
}

func TestCLIValidationDoesNotCreateData(t *testing.T) {
	for _, args := range [][]string{{""}, {"unknown"}, {"backup"}, {"restore"}, {"-trusted-proxy", "wrong"}, {"serve", "trailing"}} {
		if err := runCLI(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Fatalf("некорректная команда принята: %v", args)
		}
	}
	var output bytes.Buffer
	if err := runCLI(context.Background(), []string{"-help"}, io.Discard, &output); err != nil || output.Len() == 0 {
		t.Fatal("справка недоступна")
	}
}

func TestShareURLs(t *testing.T) {
	ips := []net.IP{net.ParseIP("192.168.1.5").To4(), net.ParseIP("10.0.0.7").To4()}

	got := shareURLs(":8080", ips)
	want := []string{"http://localhost:8080", "http://192.168.1.5:8080", "http://10.0.0.7:8080"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("все интерфейсы: ожидалось %v, получено %v", want, got)
	}

	got = shareURLs("0.0.0.0:9000", nil)
	if want := []string{"http://localhost:9000"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("без адресов в сети остаётся только localhost: %v", got)
	}

	got = shareURLs("127.0.0.1:8080", ips)
	if want := []string{"http://127.0.0.1:8080"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("конкретный адрес не подменяется: %v", got)
	}

	if got := shareURLs("нет порта", ips); got != nil {
		t.Fatalf("некорректный адрес: ожидался nil, получено %v", got)
	}
}
