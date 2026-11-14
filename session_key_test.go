package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestSessionKeyConcurrentInitializationAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "новые-данные", "retro-session.key")
	var keys [12][32]byte
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			keys[i], err = loadSessionKey(path)
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	again, err := loadSessionKey(path)
	if err != nil || again == ([32]byte{}) {
		t.Fatal("ключ не сохранился", err)
	}
	for _, k := range keys {
		if k != again {
			t.Fatal("при гонке старта выданы разные ключи")
		}
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("ключ должен быть доступен владельцу")
	}
	if err := os.WriteFile(path, []byte("ошибка"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSessionKey(path); err == nil {
		t.Fatal("повреждённый ключ должен остановить запуск")
	}
}
