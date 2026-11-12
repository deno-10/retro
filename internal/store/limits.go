package store

import (
	"context"
	"encoding/json"
	"fmt"

	"retro/internal/model"
)

const MaxBoards = 1000
const MaxBoardBytes = 4 << 20

func checkBoardCount(current, added int) error {
	if added > 0 && current+added > MaxBoards {
		return fmt.Errorf("%w: достигнут предел %d досок; обратитесь к администратору", model.ErrCapacity, MaxBoards)
	}
	return nil
}

func checkBoardSize(b *model.Board) error {
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if len(data) > MaxBoardBytes {
		return fmt.Errorf("%w: доска занимает больше %d МиБ", model.ErrCapacity, MaxBoardBytes>>20)
	}
	return nil
}

// Ready проверяет реальное чтение таблицы, а не только существование процесса.
func (s *SQLiteStore) Ready(ctx context.Context) error {
	var count int
	return s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM boards").Scan(&count)
}

func (s *JSONStore) Ready(ctx context.Context) error { return ctx.Err() }
