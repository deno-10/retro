package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"retro/internal/model"
)

func TestLiveWALBackupAndRestore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.db.Exec("PRAGMA wal_autocheckpoint=0")
	b, _ := model.NewBoard("Снимок", "classic", "owner-00000001")
	if err := s.Create(b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(b.ID, func(b *model.Board) error {
		b.Receipts = map[string]model.MutationReceipt{"owner-00000001:key-00001": {Operation: "card", Fingerprint: "saved"}}
		_, err := b.AddCard(b.Columns[0].ID, "Текст только в WAL", "", b.OwnerID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	next, err := s.CreateNext(b.ID, "Следующее", b.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "snapshot.db")
	if err := BackupSQLite(context.Background(), path, backup); err != nil {
		t.Fatal(err)
	}
	// После снимка источник изменяется, восстановление должно вернуть снимок.
	s.Update(b.ID, func(b *model.Board) error { b.Title = "Более новая версия"; return nil })
	restored := filepath.Join(dir, "restored.db")
	if err := BackupSQLite(context.Background(), backup, restored); err != nil {
		t.Fatal(err)
	}
	r, err := OpenSQLite(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := r.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Снимок" || got.Columns[0].Cards[0].Text != "Текст только в WAL" || got.NextID != next.ID || got.Revision != 3 || len(got.Receipts) != 1 {
		t.Fatalf("снимок потерял данные: %+v", got)
	}
	if n, err := r.Get(next.ID); err != nil || n.PrevID != b.ID {
		t.Fatalf("связь ретро потеряна: %v", err)
	}
	info, _ := os.Stat(backup)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("права снимка: %v", info.Mode())
	}
}

func TestBackupRefusesMissingCorruptAndExistingFiles(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	dest := filepath.Join(dir, "dest.db")
	if err := BackupSQLite(context.Background(), missing, dest); err == nil {
		t.Fatal("отсутствующая БД принята")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("создана пустая БД вместо отсутствующей")
	}
	if err := VerifySQLite(context.Background(), missing); err == nil {
		t.Fatal("отсутствующая БД прошла проверку")
	}
	corrupt := filepath.Join(dir, "corrupt.db")
	os.WriteFile(corrupt, []byte("повреждённая база"), 0o600)
	if err := BackupSQLite(context.Background(), corrupt, dest); err == nil {
		t.Fatal("повреждённая БД принята")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("неудачный снимок опубликован")
	}
	source := filepath.Join(dir, "source.db")
	s, _ := OpenSQLite(source)
	b, _ := model.NewBoard("Доска", "classic", "owner-00000001")
	s.Create(b)
	defer s.Close()
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		t.Run("назначение"+suffix, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dest.db")
			os.WriteFile(path+suffix, []byte("сохранить"), 0o600)
			if err := BackupSQLite(context.Background(), source, path); err == nil {
				t.Fatal("существующее назначение принято")
			}
			got, _ := os.ReadFile(path + suffix)
			if string(got) != "сохранить" {
				t.Fatal("файл назначения перезаписан")
			}
		})
	}
	s.db.Exec("UPDATE boards SET data = '{}' WHERE id = ?", b.ID)
	if err := BackupSQLite(context.Background(), source, dest); err == nil {
		t.Fatal("повреждённая доска прошла проверку снимка")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("некорректный снимок опубликован")
	}
}
