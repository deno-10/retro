package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"retro/internal/model"
)

func TestSQLiteStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "retro.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := model.NewBoard("Спринт 3", "classic", "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(b); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(b); err == nil {
		t.Fatal("повторное создание доски с тем же id должно давать ошибку")
	}
	updated, err := s.Update(b.ID, func(bd *model.Board) error {
		_, err := bd.AddCard(bd.Columns[0].ID, "текст", "", "alice")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.UpdatedAt.After(b.UpdatedAt) {
		t.Fatal("Update должен обновлять UpdatedAt")
	}

	// Get возвращает копию: правки снаружи не протекают в хранилище.
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Title = "подмена"
	again, err := s.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Title != "Спринт 3" {
		t.Fatal("Get должен возвращать копию доски")
	}

	// Ошибка внутри Update откатывает транзакцию и возвращается как есть.
	if _, err := s.Update(b.ID, func(bd *model.Board) error {
		bd.Title = "не должно сохраниться"
		return model.ErrForbidden
	}); !errors.Is(err, model.ErrForbidden) {
		t.Fatalf("ожидался ErrForbidden, получено %v", err)
	}
	if cur, _ := s.Get(b.ID); cur.Title != "Спринт 3" {
		t.Fatal("неудачный Update не должен менять доску")
	}
	if _, err := s.Update("nope", func(*model.Board) error { return nil }); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("Update несуществующей доски: ожидался ErrNotFound, получено %v", err)
	}
	if _, err := s.Get("nope"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("ожидался ErrNotFound, получено %v", err)
	}

	// После Close и повторного Open данные на месте.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	r, err := reopened.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Columns[0].Cards) != 1 || r.Columns[0].Cards[0].Text != "текст" {
		t.Fatalf("после перезапуска данные потеряны: %+v", r)
	}
	if empty, err := reopened.Empty(); err != nil || empty {
		t.Fatalf("база с доской не должна считаться пустой: empty=%v err=%v", empty, err)
	}
}

func TestSQLiteEmpty(t *testing.T) {
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "retro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if empty, err := s.Empty(); err != nil || !empty {
		t.Fatalf("новая база должна быть пустой: empty=%v err=%v", empty, err)
	}
}

func TestImportJSON(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "boards.json")

	// Готовим старый JSON-файл через JSONStore.
	js, err := OpenJSON(jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, title := range []string{"Спринт 1", "Спринт 2"} {
		b, err := model.NewBoard(title, "ssc", "owner-1")
		if err != nil {
			t.Fatal(err)
		}
		if err := js.Create(b); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, b.ID)
	}

	s, err := OpenSQLite(filepath.Join(dir, "retro.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n, err := ImportJSON(s, jsonPath)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("ожидалось 2 импортированные доски, получено %d", n)
	}
	for i, id := range ids {
		b, err := s.Get(id)
		if err != nil {
			t.Fatalf("доска %s после импорта не найдена: %v", id, err)
		}
		if b.Template != "ssc" || len(b.Columns) != 3 {
			t.Fatalf("доска %d импортирована с потерями: %+v", i, b)
		}
	}

	// Отсутствующий файл — не ошибка, ноль досок.
	if n, err := ImportJSON(s, filepath.Join(dir, "missing.json")); err != nil || n != 0 {
		t.Fatalf("отсутствующий файл: n=%d err=%v", n, err)
	}
	// Повторный импорт упирается в существующие id.
	if _, err := ImportJSON(s, jsonPath); err == nil {
		t.Fatal("повторный импорт тех же досок должен давать ошибку")
	}
}

func TestOpenByExtension(t *testing.T) {
	dir := t.TempDir()
	js, err := Open(filepath.Join(dir, "boards.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := js.(*JSONStore); !ok {
		t.Fatalf("для .json ожидался JSONStore, получен %T", js)
	}
	js.Close()

	dbPath := filepath.Join(dir, "retro.db")
	sq, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sq.(*SQLiteStore); !ok {
		t.Fatalf("для .db ожидался SQLiteStore, получен %T", sq)
	}
	sq.Close()
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("файл базы должен быть создан: %v", err)
	}
}
