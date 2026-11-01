package store

import (
	"path/filepath"
	"testing"

	"retro/internal/model"
)

func TestJSONStorePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "boards.json")
	s, err := OpenJSON(path)
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
	if _, err := s.Update(b.ID, func(bd *model.Board) error {
		_, err := bd.AddCard(bd.Columns[0].ID, "текст", "", "alice")
		return err
	}); err != nil {
		t.Fatal(err)
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

	// Ошибка внутри Update ничего не меняет.
	if _, err := s.Update(b.ID, func(bd *model.Board) error {
		bd.Title = "не должно сохраниться"
		return model.ErrForbidden
	}); err != model.ErrForbidden {
		t.Fatalf("ожидался ErrForbidden, получено %v", err)
	}
	if cur, _ := s.Get(b.ID); cur.Title != "Спринт 3" {
		t.Fatal("неудачный Update не должен менять доску")
	}

	reopened, err := OpenJSON(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := reopened.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Columns[0].Cards) != 1 || r.Columns[0].Cards[0].Text != "текст" {
		t.Fatalf("после перезапуска данные потеряны: %+v", r)
	}
	if _, err := reopened.Get("nope"); err != model.ErrNotFound {
		t.Fatalf("ожидался ErrNotFound, получено %v", err)
	}
}
