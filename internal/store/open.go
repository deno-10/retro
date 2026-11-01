package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"retro/internal/model"
)

// Open выбирает реализацию по расширению файла: .json — JSONStore (прежний
// формат, обратная совместимость), всё остальное — SQLite.
func Open(path string) (Store, error) {
	if strings.EqualFold(filepath.Ext(path), ".json") {
		return OpenJSON(path)
	}
	return OpenSQLite(path)
}

// ImportJSON читает доски из файла в формате JSONStore и добавляет их в dst.
// Возвращает число добавленных досок. Отсутствующий или пустой файл — ноль
// досок без ошибки.
func ImportJSON(dst Store, path string) (int, error) {
	boards, err := readJSONBoards(path)
	if err != nil {
		return 0, err
	}
	for i, b := range boards {
		if err := dst.Create(b); err != nil {
			return i, fmt.Errorf("импорт доски %s: %w", b.ID, err)
		}
	}
	return len(boards), nil
}

// readJSONBoards разбирает файл JSONStore. Отсутствующий файл — пустой список.
func readJSONBoards(path string) ([]*model.Board, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	var list []*model.Board
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("разбор %s: %w", path, err)
	}
	return list, nil
}
