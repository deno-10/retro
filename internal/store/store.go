// Package store — хранилище досок. Интерфейс Store один, реализации
// подменяются: SQLite (по умолчанию), JSON-файл (прежний формат) и PostgreSQL.
package store

import (
	"encoding/json"
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
	// CreateMany сохраняет все доски либо не сохраняет ни одной.
	CreateMany(boards []*model.Board) error
	// CreateNext атомарно создаёт следующее ретро и связывает обе доски.
	CreateNext(prevID, title, actorID string) (*model.Board, error)
	Get(id string) (*model.Board, error)
	// GetForMember возвращает ErrNotFound и для чужой, и для отсутствующей доски.
	GetForMember(id, participant string) (*model.Board, error)
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
	list, err := readJSONBoards(path)
	if err != nil {
		return nil, err
	}
	for _, b := range list {
		s.boards[b.ID] = b
	}
	return s, nil
}

// Create сохраняет новую доску.
func (s *JSONStore) Create(b *model.Board) error {
	return s.CreateMany([]*model.Board{b})
}

// CreateMany добавляет доски одним снимком; ошибка не меняет память и файл.
func (s *JSONStore) CreateMany(boards []*model.Board) error {
	if len(boards) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkBoardCount(len(s.boards), len(boards)); err != nil {
		return err
	}
	added := make(map[string]*model.Board, len(boards))
	for _, b := range boards {
		if b == nil {
			return fmt.Errorf("%w: доска не задана", model.ErrInvalid)
		}
		if s.boards[b.ID] != nil || added[b.ID] != nil {
			return fmt.Errorf("доска %s уже существует", b.ID)
		}
		if err := checkBoardSize(b); err != nil {
			return err
		}
		c, err := clone(b)
		if err != nil {
			return err
		}
		added[b.ID] = c
	}
	for id, b := range added {
		s.boards[id] = b
	}
	if err := s.persistLocked(); err != nil {
		for id := range added {
			delete(s.boards, id)
		}
		return err
	}
	return nil
}

// CreateNext проверяет права и сохраняет обе доски под одной блокировкой.
func (s *JSONStore) CreateNext(prevID, title, actorID string) (*model.Board, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.boards[prevID]
	if !ok {
		return nil, model.ErrNotFound
	}
	prev, err := clone(current)
	if err != nil {
		return nil, err
	}
	next, err := prev.NextBoard(title, actorID)
	if err != nil {
		return nil, err
	}
	if err := checkBoardCount(len(s.boards), 1); err != nil {
		return nil, err
	}
	if err := checkBoardSize(next); err != nil {
		return nil, err
	}
	if _, exists := s.boards[next.ID]; exists {
		return nil, fmt.Errorf("доска %s уже существует", next.ID)
	}
	savedNext, err := clone(next)
	if err != nil {
		return nil, err
	}
	prev.NextID = next.ID
	prev.Revision = current.Revision + 1
	prev.UpdatedAt = time.Now()
	s.boards[prevID], s.boards[next.ID] = prev, savedNext
	if err := s.persistLocked(); err != nil {
		s.boards[prevID] = current
		delete(s.boards, next.ID)
		return nil, err
	}
	return next, nil
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

func (s *JSONStore) GetForMember(id, participant string) (*model.Board, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.boards[id]
	if !ok || !b.HasMember(participant) {
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
	work.Revision = current.Revision + 1
	work.UpdatedAt = time.Now()
	if err := checkBoardSize(work); err != nil {
		return nil, err
	}
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
