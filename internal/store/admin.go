package store

import (
	"context"
	"fmt"
	"regexp"

	"retro/internal/model"
)

// TransferOwner доступна только через локальный CLI администратора, не HTTP.
// Нужна после потери browser storage. Смена владельца не меняет авторов карточек.
func TransferOwner(ctx context.Context, path, boardID, participantID string) error {
	if !regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`).MatchString(participantID) || boardID == "" {
		return fmt.Errorf("%w: укажите доску и корректный ID нового ведущего", model.ErrInvalid)
	}
	db, err := openExistingMode(path, "rw")
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	b, err := readBoard(tx, boardID)
	if err != nil {
		return err
	}
	b.OwnerID = participantID
	b.Revision++
	if err := updateBoard(tx, boardID, b); err != nil {
		return err
	}
	return tx.Commit()
}
