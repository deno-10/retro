package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"retro/internal/model"
)

// PostgresStore разделяется несколькими экземплярами Retro. Блокировка строки
// защищает все операции над одной доской, включая готовность, таймеры и Jira.
type PostgresStore struct{ db *sql.DB }

const postgresSchema = `
CREATE TABLE IF NOT EXISTS retro_boards (
 id TEXT PRIMARY KEY,
 data JSONB NOT NULL,
 created_at TIMESTAMPTZ NOT NULL,
 updated_at TIMESTAMPTZ NOT NULL,
 timer_ends_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS retro_boards_timer ON retro_boards(timer_ends_at)
 WHERE timer_ends_at IS NOT NULL;`

const postgresTimeout = 5 * time.Second

// OpenPostgres не меняет схему: DDL выполняется отдельной командой migrate.
// DSN не попадает в ошибки и логи, в том числе при ошибке разбора/соединения.
func OpenPostgres(dsn string) (*PostgresStore, error) {
	if dsn == "" {
		return nil, errors.New("RETRO_DATABASE_URL не задан")
	}
	cfg, err := pq.NewConfig(dsn)
	if err != nil {
		return nil, errors.New("неверный RETRO_DATABASE_URL")
	}
	cfg.ConnectTimeout = postgresTimeout
	connector, err := pq.NewConnectorConfig(cfg)
	if err != nil {
		return nil, errors.New("неверный RETRO_DATABASE_URL")
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	s := &PostgresStore{db: db}
	ctx, cancel := context.WithTimeout(context.Background(), postgresTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, pgError(err)
	}
	return s, nil
}

// pgError оставляет диагностический SQLSTATE, но не DSN, SQL и данные карточек.
func pgError(err error) error {
	if err == nil {
		return nil
	}
	var e *pq.Error
	if errors.As(err, &e) {
		return fmt.Errorf("ошибка PostgreSQL (SQLSTATE %s)", e.Code)
	}
	return errors.New("PostgreSQL недоступен или время операции истекло")
}

func (s *PostgresStore) Migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return pgError(err)
	}
	defer tx.Rollback()
	// Отдельный ключ сериализует повторные/параллельные запуски миграции.
	if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(726574, 1)"); err != nil {
		return pgError(err)
	}
	if _, err = tx.ExecContext(ctx, postgresSchema); err != nil {
		return pgError(err)
	}
	return pgError(tx.Commit())
}

func (s *PostgresStore) Ready(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT id,data,created_at,updated_at,timer_ends_at FROM retro_boards LIMIT 0")
	if err != nil {
		return pgError(err)
	}
	return pgError(rows.Close())
}
func (s *PostgresStore) Close() error                { return s.db.Close() }
func (s *PostgresStore) Create(b *model.Board) error { return s.CreateMany([]*model.Board{b}) }

// Квота создания сериализуется между процессами. Ключ берём до блокировки
// доски в CreateNext, чтобы порядок блокировок оставался одинаковым.
func pgCapacity(ctx context.Context, tx *sql.Tx, added int) error {
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(726574, 0)"); err != nil {
		return pgError(err)
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM retro_boards").Scan(&n); err != nil {
		return pgError(err)
	}
	return checkBoardCount(n, added)
}

func (s *PostgresStore) CreateMany(boards []*model.Board) error {
	if len(boards) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return pgError(err)
	}
	defer tx.Rollback()
	if err = pgCapacity(ctx, tx, len(boards)); err != nil {
		return err
	}
	for _, b := range boards {
		if err = pgInsert(ctx, tx, b); err != nil {
			return err
		}
	}
	return pgError(tx.Commit())
}

func pgInsert(ctx context.Context, tx *sql.Tx, b *model.Board) error {
	if b == nil {
		return fmt.Errorf("%w: доска не задана", model.ErrInvalid)
	}
	if err := checkBoardSize(b); err != nil {
		return err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO retro_boards(id,data,created_at,updated_at,timer_ends_at) VALUES($1,$2,$3,$4,$5)", b.ID, string(data), b.CreatedAt, b.UpdatedAt, b.TimerEndsAt)
	return pgError(err)
}

func pgRead(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string, lock bool) (*model.Board, error) {
	query := "SELECT data FROM retro_boards WHERE id=$1"
	if lock {
		query += " FOR UPDATE"
	}
	return scanPostgresBoard(q.QueryRowContext(ctx, query, id))
}

func scanPostgresBoard(row *sql.Row) (*model.Board, error) {
	var data string
	err := row.Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, model.ErrNotFound
	}
	if err != nil {
		return nil, pgError(err)
	}
	return decodeBoard(data)
}

func (s *PostgresStore) Get(id string) (*model.Board, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresTimeout)
	defer cancel()
	return pgRead(ctx, s.db, id, false)
}

func (s *PostgresStore) GetForMember(id, participant string) (*model.Board, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresTimeout)
	defer cancel()
	return scanPostgresBoard(s.db.QueryRowContext(ctx, `SELECT data FROM retro_boards
		WHERE id=$1 AND $2 <> '' AND (data->>'ownerId'=$2 OR (data->'members') ? $2)`, id, participant))
}

func pgSave(ctx context.Context, tx *sql.Tx, id string, b *model.Board) error {
	b.UpdatedAt = time.Now()
	if err := checkBoardSize(b); err != nil {
		return err
	}
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE retro_boards SET data=$2,updated_at=$3,timer_ends_at=$4 WHERE id=$1", id, string(data), b.UpdatedAt, b.TimerEndsAt)
	return pgError(err)
}

func (s *PostgresStore) Update(id string, fn func(*model.Board) error) (*model.Board, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, pgError(err)
	}
	defer tx.Rollback()
	b, err := pgRead(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	revision := b.Revision
	if err = fn(b); err != nil {
		return nil, err
	}
	b.Revision = revision + 1
	if err = pgSave(ctx, tx, id, b); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, pgError(err)
	}
	return b, nil
}

func (s *PostgresStore) CreateNext(id, title, actor string) (*model.Board, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresTimeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, pgError(err)
	}
	defer tx.Rollback()
	if err = pgCapacity(ctx, tx, 1); err != nil {
		return nil, err
	}
	prev, err := pgRead(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	next, err := prev.NextBoard(title, actor)
	if err != nil {
		return nil, err
	}
	prev.NextID, prev.Revision = next.ID, prev.Revision+1
	if err = pgSave(ctx, tx, id, prev); err != nil {
		return nil, err
	}
	if err = pgInsert(ctx, tx, next); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, pgError(err)
	}
	return next, nil
}

// DueTimers позволяет восстановить истёкшие таймеры без открытых браузеров.
func (s *PostgresStore) DueTimers(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM retro_boards WHERE timer_ends_at <= $1 ORDER BY timer_ends_at LIMIT 100", now)
	if err != nil {
		return nil, pgError(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, pgError(err)
		}
		ids = append(ids, id)
	}
	return ids, pgError(rows.Err())
}
