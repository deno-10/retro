package server

import (
	"database/sql"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"testing/fstest"

	"retro/internal/hub"
	"retro/internal/model"
	"retro/internal/store"
)

func TestNextBoardFailureDoesNotRequireCompensatingWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retro.db")
	st, err := store.OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	prev, err := model.NewBoard("Предыдущее ретро", "classic", "owner-00000001")
	if err != nil {
		t.Fatal(err)
	}
	prev.Phase = model.PhaseDiscuss
	if err := st.Create(prev); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Новая доска не записывается, а отдельная компенсирующая запись тоже
	// невозможна. Откат одной транзакции не выполняет второй UPDATE и безопасен.
	for _, query := range []string{
		`CREATE TRIGGER fail_next BEFORE INSERT ON boards
		 WHEN json_extract(NEW.data, '$.prevId') IS NOT NULL
		 BEGIN SELECT RAISE(ABORT, 'ошибка создания следующей доски'); END`,
		`CREATE TRIGGER fail_compensation BEFORE UPDATE ON boards
		 WHEN json_extract(OLD.data, '$.nextId') IS NOT NULL
		 AND json_extract(NEW.data, '$.nextId') IS NULL
		 BEGIN SELECT RAISE(ABORT, 'ошибка компенсирующей записи'); END`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	static := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}}
	h := New(st, hub.New(), static, slog.New(slog.NewTextHandler(io.Discard, nil)), testOptions())
	rec, _ := do(t, h, "POST", "/api/boards/"+prev.ID+"/next", prev.OwnerID, map[string]any{"title": "Следующее ретро"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("ошибка хранилища должна вернуть 500: %d %s", rec.Code, rec.Body.String())
	}
	_, view := do(t, h, "GET", "/api/boards/"+prev.ID, prev.OwnerID, nil)
	if view["nextId"] != nil {
		t.Fatalf("после ошибки осталась ссылка на отсутствующую доску: %v", view["nextId"])
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM boards`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("неудачное создание оставило лишние доски: %d", count)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_next`); err != nil {
		t.Fatal(err)
	}
	rec, next := do(t, h, "POST", "/api/boards/"+prev.ID+"/next", prev.OwnerID, map[string]any{"title": "Следующее ретро"})
	if rec.Code != http.StatusCreated || next["prevId"] != prev.ID {
		t.Fatalf("после устранения ошибки создание должно пройти: %d %s", rec.Code, rec.Body.String())
	}
}
