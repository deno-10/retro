package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // драйвер database/sql с именем "sqlite"

	"retro/internal/model"
)

// SQLiteStore хранит каждую доску JSON-документом в строке таблицы boards.
// Драйвер modernc.org/sqlite написан на чистом Go, CGO не нужен, бинарник
// по-прежнему один. Схема нарочно примитивная: доска целиком в колонке data,
// а id и даты продублированы колонками для ключа и будущих выборок.
type SQLiteStore struct {
	db *sql.DB
}

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS boards (
	id         TEXT PRIMARY KEY,
	data       TEXT NOT NULL,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
)`

// OpenSQLite открывает базу по пути path, создавая файл и таблицу при
// необходимости. Отсутствующая база — нормальный первый запуск.
func OpenSQLite(path string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("sqlite %s: %w", path, err)
	}
	// Одно соединение: PRAGMA busy_timeout действует на соединение, а не на
	// базу, и единственный писатель внутри процесса не ловит SQLITE_BUSY от
	// самого себя. Для нагрузки ретро-досок этого более чем достаточно.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	for _, q := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		sqliteSchema,
	} {
		if _, err := db.Exec(q); err != nil {
			db.Close()
			return nil, fmt.Errorf("sqlite %s: %w", path, err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

// Create сохраняет новую доску. Повторный id — ошибка, как и в JSONStore.
func (s *SQLiteStore) Create(b *model.Board) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO boards (id, data, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		b.ID, string(data), ts(b.CreatedAt), ts(b.UpdatedAt),
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("доска %s уже существует", b.ID)
	}
	return nil
}

// Get возвращает копию доски: объект собран из JSON только что и ни с кем
// не разделяется, менять его снаружи безопасно.
func (s *SQLiteStore) Get(id string) (*model.Board, error) {
	var data string
	err := s.db.QueryRow(`SELECT data FROM boards WHERE id = ?`, id).Scan(&data)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, model.ErrNotFound
	case err != nil:
		return nil, err
	}
	return decodeBoard(data)
}

// Update читает доску в транзакции, применяет fn и записывает результат.
// Если fn вернула ошибку, транзакция откатывается, в базе ничего не меняется,
// а ошибка возвращается как есть — errors.Is с ошибками model работает.
func (s *SQLiteStore) Update(id string, fn func(b *model.Board) error) (*model.Board, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // после Commit откат — no-op

	var data string
	err = tx.QueryRow(`SELECT data FROM boards WHERE id = ?`, id).Scan(&data)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, model.ErrNotFound
	case err != nil:
		return nil, err
	}
	b, err := decodeBoard(data)
	if err != nil {
		return nil, err
	}
	if err := fn(b); err != nil {
		return nil, err
	}
	b.UpdatedAt = time.Now()
	out, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`UPDATE boards SET data = ?, updated_at = ? WHERE id = ?`,
		string(out), ts(b.UpdatedAt), id,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// b — свежий объект из этой транзакции, вовне он больше не нужен,
	// поэтому отдаём его как копию.
	return b, nil
}

// Empty сообщает, что в базе нет ни одной доски. Нужен для разового импорта
// из старого JSON-файла при первом запуске.
func (s *SQLiteStore) Empty() (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM boards`).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// Close закрывает базу.
func (s *SQLiteStore) Close() error { return s.db.Close() }

func decodeBoard(data string) (*model.Board, error) {
	var b model.Board
	if err := json.Unmarshal([]byte(data), &b); err != nil {
		return nil, fmt.Errorf("разбор доски из базы: %w", err)
	}
	return &b, nil
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
