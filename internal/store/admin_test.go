package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"retro/internal/model"
)

func TestOwnerRecoveryPreservesDataAndRejectsInvalidInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "retro.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b, _ := model.NewBoard("Восстановление ведущего", "classic", "owner-00000001")
	b.AddCard(b.Columns[0].ID, "Не менять", "", b.OwnerID)
	if err := s.Create(b); err != nil {
		t.Fatal(err)
	}
	if err := TransferOwner(context.Background(), path, b.ID, "invalid!"); !errors.Is(err, model.ErrInvalid) {
		t.Fatal(err)
	}
	if err := TransferOwner(context.Background(), path, "missing", "owner-00000002"); !errors.Is(err, model.ErrNotFound) {
		t.Fatal(err)
	}
	got, _ := s.Get(b.ID)
	if got.OwnerID != b.OwnerID || got.Revision != b.Revision {
		t.Fatal("отказ изменил доску")
	}
	if err := TransferOwner(context.Background(), path, b.ID, "owner-00000002"); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(b.ID)
	if got.OwnerID != "owner-00000002" || got.Revision != b.Revision+1 || got.Columns[0].Cards[0].AuthorID != b.OwnerID || got.Columns[0].Cards[0].Text != "Не менять" {
		t.Fatalf("смена владельца повредила данные: %+v", got)
	}
}
