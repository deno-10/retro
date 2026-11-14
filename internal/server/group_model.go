package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"retro/internal/model"
	"retro/internal/ollama"
)

func (s *Server) writeSuggestion(w http.ResponseWriter, rev uint64, columns []model.ColumnGroups, source string) {
	name := ""
	if s.options.Ollama != nil {
		name = s.options.Ollama.Model()
	}
	writeJSON(w, http.StatusOK, struct {
		Revision uint64               `json:"revision"`
		Columns  []model.ColumnGroups `json:"columns"`
		Source   string               `json:"source"`
		Model    string               `json:"model,omitempty"`
	}{rev, columns, source, name})
}

func (s *Server) handleModelSuggestions(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision *uint64 `json:"revision"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	if in.Revision == nil {
		s.writeErr(w, fmt.Errorf("%w: укажите версию доски", model.ErrInvalid))
		return
	}
	id, actor := r.PathValue("id"), participant(r)
	check := func() (*model.Board, error) {
		b, err := s.currentBoard(id, actor)
		if err != nil {
			return nil, err
		}
		if err := b.CheckGroupReview(actor); err != nil {
			return nil, err
		}
		if b.Revision != *in.Revision {
			return nil, fmt.Errorf("%w: доска изменилась, обновите предложения", model.ErrConflict)
		}
		return b, nil
	}
	b, err := check()
	if err != nil {
		s.writeErr(w, err)
		return
	}
	client := s.options.Ollama
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Локальная модель не настроена."})
		return
	}
	select {
	case s.modelSlot <- struct{}{}:
		defer func() { <-s.modelSlot }()
	default:
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Qwen занят другой доской. Попробуйте чуть позже."})
		return
	}
	// Первая загрузка весов может занять дольше обычного HTTP-запроса.
	const timeout = 90 * time.Second
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout + s.options.WriteTimeout))
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	columns := []model.ColumnGroups{}
	called := false
	for _, col := range b.Columns {
		var ids, texts []string
		for _, card := range col.Cards {
			if card.GroupID == "" {
				ids, texts = append(ids, card.ID), append(texts, card.Text)
			}
		}
		if len(ids) < 2 {
			continue
		}
		if called {
			if _, err := check(); err != nil {
				s.writeErr(w, err)
				return
			}
		}
		groups, err := client.Groups(ctx, texts)
		called = true
		if err != nil {
			if errors.Is(err, ollama.ErrSize) {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "В колонке слишком много карточек или текста для локальной модели. Можно использовать словарь."})
			} else {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Не удалось получить ответ Qwen. Можно повторить запрос или использовать словарь."})
			}
			return
		}
		result := model.ColumnGroups{ColumnID: col.ID}
		for _, group := range groups {
			mapped := make([]string, len(group))
			for i, n := range group {
				mapped[i] = ids[n]
			}
			result.Groups = append(result.Groups, mapped)
		}
		if len(result.Groups) > 0 {
			columns = append(columns, result)
		}
	}
	// Пока модель отвечала, могли смениться ведущий, фаза или сами карточки.
	if _, err := check(); err != nil {
		s.writeErr(w, err)
		return
	}
	s.writeSuggestion(w, b.Revision, columns, "qwen")
}
