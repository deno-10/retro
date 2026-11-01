// Package store — хранилище досок. Интерфейс Store один, реализации
// подменяются: сейчас JSON-файл, дальше SQLite и Postgres.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"retro/internal/model"
)

// Store — то, что нужно HTTP-слою от хранилища.
// Get возвращает копию: менять её безопасно, в хранилище это не попадёт.
// Update применяет fn к доске атомарно: либо вся мутация, либо ничего.
type Store interface {
	Create(b *model.Board) error
	Get(id string) (*model.Board, error)
	Update(id string, fn func(b *model.Board) error) (*model.Board, error)
	Close() error
}

// JSONStore держит доски в памяти и после каждого изменения сбрасывает
// снимок в файл (запись во временный файл + rename, чтобы не потерять данные
// при падении посреди записи). Годится для MVP; на десятках досок в секунду
// упрётся в диск — тогда SQLite.
type JSONStore struct {
	mu     sync.RWMutex
	path   string
	boards map[string]*model.Board
}

// OpenJSON открывает хранилище. Отсутствующий файл — нормальный первый запуск.
func OpenJSON(path string) (*JSONStore, error) {
	s := &JSONStore{path: path, boards: map[string]*model.Board{}}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	if len(data) == 0 {
		return s, nil
	}
	var list []*model.Board
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("разбор %s: %w", path, err)
	}
	for _, b := range list {
		s.boards[b.ID] = b
	}
	return s, nil
}

// Create сохраняет новую доску.
func (s *JSONStore) Create(b *model.Board) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.boards[b.ID]; exists {
		return fmt.Errorf("доска %s уже существует", b.ID)
	}
	c, err := clone(b)
	if err != nil {
		return err
	}
	s.boards[b.ID] = c
	if err := s.persistLocked(); err != nil {
		delete(s.boards, b.ID)
		return err
	}
	return nil
}

// Get возвращает копию доски.
func (s *JSONStore) Get(id string) (*model.Board, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.boards[id]
	if !ok {
		return nil, model.ErrNotFound
	}
	return clone(b)
}

// Update применяет fn к копии доски и, если fn не вернула ошибку,
// подменяет доску в хранилище и записывает снимок на диск.
func (s *JSONStore) Update(id string, fn func(b *model.Board) error) (*model.Board, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.boards[id]
	if !ok {
		return nil, model.ErrNotFound
	}
	work, err := clone(current)
	if err != nil {
		return nil, err
	}
	if err := fn(work); err != nil {
		return nil, err
	}
	work.UpdatedAt = time.Now()
	s.boards[id] = work
	if err := s.persistLocked(); err != nil {
		s.boards[id] = current
		return nil, err
	}
	return clone(work)
}

// Close ничего не делает: файл закрывается после каждой записи.
// Метод нужен ради интерфейса — у SQLite будет что закрывать.
func (s *JSONStore) Close() error { return nil }

func (s *JSONStore) persistLocked() error {
	list := make([]*model.Board, 0, len(s.boards))
	for _, b := range s.boards {
		list = append(list, b)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt.Before(list[j].CreatedAt) })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func clone(b *model.Board) (*model.Board, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	var c model.Board
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
