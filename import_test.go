package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"retro/internal/model"
	"retro/internal/store"
)

func TestOpenStoreRetriesFailedImportAfterRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retro.db")
	var boards []*model.Board
	for _, title := range []string{"Первая", "Вторая", "Третья"} {
		b, err := model.NewBoard(title, "classic", "owner-1")
		if err != nil {
			t.Fatal(err)
		}
		boards = append(boards, b)
	}
	data, err := json.Marshal(boards)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "boards.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_second_import BEFORE INSERT ON boards
		WHEN json_extract(NEW.data, '$.title') = 'Вторая'
		BEGIN SELECT RAISE(ABORT, 'ошибка второй записи'); END`); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if opened, err := openStore(path, logger); err == nil {
		opened.Close()
		t.Fatal("ожидалась ошибка второй записи импорта")
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM boards`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("неудачный импорт оставил %d досок и заблокировал повторный запуск", count)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_second_import`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Новый вызов openStore должен увидеть пустую БД и повторить весь импорт.
	reopened, err := openStore(path, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for _, b := range boards {
		got, err := reopened.Get(b.ID)
		if err != nil {
			t.Fatalf("после перезапуска отсутствует доска %q: %v", b.Title, err)
		}
		if got.Title != b.Title {
			t.Fatalf("название доски изменилось: ожидалось %q, получено %q", b.Title, got.Title)
		}
	}
}
