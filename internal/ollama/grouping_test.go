package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestLocalOnly(t *testing.T) {
	for _, pair := range [][2]string{{"qwen3:4b", "10.0.0.5:11434"}, {"qwen3:4b", "example.org:11434"}, {"qwen3:4b", "127.0.0.1:0"}, {"qwen3:cloud", ""}, {"", "127.0.0.1:11434"}} {
		if _, err := New(pair[0], pair[1]); err == nil {
			t.Fatal("принята небезопасная или неполная конфигурация:", pair)
		}
	}
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("текст переслан по редиректу")
	}))
	defer other.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL, 307) }))
	defer ts.Close()
	client, err := New("qwen3:4b", strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Groups(t.Context(), []string{"один", "два"}); err == nil {
		t.Fatal("принят редирект")
	}
}

func TestValidateLocalResponse(t *testing.T) {
	for _, tc := range []struct {
		content string
		valid   bool
	}{
		{`{"groups":[[2,1]]}`, true}, {`{"groups":[]}`, true}, {`{}`, false}, {`{"groups":null}`, false},
		{`{"groups":[[1,3]]}`, false}, {`{"groups":[[1,2],[2,1]]}`, false}, {`{"groups":[[1]]}`, false}, {`{"groups":[[1,1]]}`, false},
	} {
		t.Run(tc.content, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"done": true, "message": map[string]string{"content": tc.content}})
			}))
			defer ts.Close()
			client, err := New("qwen3:4b", strings.TrimPrefix(ts.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			groups, err := client.Groups(t.Context(), []string{"А", "Б"})
			if (err == nil) != tc.valid {
				t.Fatalf("группы: %v, ошибка: %v", groups, err)
			}
			if strings.Contains(tc.content, "[2,1]") && tc.valid && !reflect.DeepEqual(groups, [][]int{{1, 0}}) {
				t.Fatalf("неверное соответствие: %v", groups)
			}
		})
	}
	client, err := New("qwen3:4b", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Groups(context.Background(), []string{strings.Repeat("А", 4000), "Б"}); err != ErrSize {
		t.Fatal("чрезмерно большой ввод должен отклоняться до запроса:", err)
	}
}
