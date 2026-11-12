package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// openExisting не создаёт отсутствующую БД и не меняет её схему/режим журнала.
func openExisting(path string) (*sql.DB, error) {
	return openExistingMode(path, "ro")
}

func openExistingMode(path, mode string) (*sql.DB, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, errors.New("ожидается существующий непустой файл SQLite")
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", mode)
	q.Set("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// VerifySQLite проверяет страницы SQLite и декодирование всех досок.
// Неверный путь никогда не превращается в новую пустую базу.
func VerifySQLite(ctx context.Context, path string) error {
	db, err := openExisting(path)
	if err != nil {
		return err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("нарушена целостность SQLite: %s", integrity)
	}
	rows, err := db.QueryContext(ctx, "SELECT id, data FROM boards")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return err
		}
		b, err := decodeBoard(data)
		if err != nil {
			return err
		}
		if b.ID != id || b.OwnerID == "" || !b.Phase.Valid() {
			return fmt.Errorf("доска %s содержит некорректные данные", id)
		}
	}
	return rows.Err()
}

// BackupSQLite создаёт согласованный снимок живой WAL-базы. Имя назначения
// публикуется только после проверки и fsync; существующие файлы не заменяются.
// Та же операция восстанавливает снимок в новый путь без старых WAL/SHM.
func BackupSQLite(ctx context.Context, source, dest string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(dest + suffix); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				return fmt.Errorf("назначение уже существует: %s", dest+suffix)
			}
			return err
		}
	}
	db, err := openExisting(source)
	if err != nil {
		return err
	}
	defer db.Close()
	file, err := os.CreateTemp(filepath.Dir(dest), ".retro-snapshot-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	if err := file.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	defer os.Remove(tmp)
	if _, err := db.ExecContext(ctx, "VACUUM main INTO ?", tmp); err != nil {
		return fmt.Errorf("снимок SQLite: %w", err)
	}
	if err := VerifySQLite(ctx, tmp); err != nil {
		return fmt.Errorf("проверка снимка: %w", err)
	}
	file, err = os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = file.Sync()
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Link(tmp, dest); err != nil { // атомарно, без перезаписи
		return err
	}
	dir, err := os.Open(filepath.Dir(dest))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
