package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"retro/internal/jira"
	"retro/internal/model"
)

func (s *Server) view(b *model.Board, pid string) model.BoardView {
	v := b.View(pid)
	if c := s.options.Jira; c != nil {
		v.Jira = &model.JiraConfigView{Project: c.Project(), IssueType: c.IssueType()}
	}
	return v
}

func (s *Server) jiraSlot(w http.ResponseWriter) bool {
	if s.options.Jira == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Jira не подключена. Администратору нужно настроить адрес, проект и токен на сервере Retro."})
		return false
	}
	select {
	case s.jiraSlots <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "5")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "Jira уже обрабатывает несколько запросов. Повторите позже."})
		return false
	}
}

func (s *Server) handleCreateJira(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Revision *uint64 `json:"revision"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	if in.Revision == nil {
		s.writeErr(w, fmt.Errorf("%w: укажите версию выбранного решения", model.ErrInvalid))
		return
	}
	if !s.jiraSlot(w) {
		return
	}
	defer func() { <-s.jiraSlots }()
	c := s.options.Jira
	label := "retro-" + model.NewID()
	actionID := r.PathValue("actionId")
	var input jira.Input
	started := false
	b, pid, err := s.updateBoard(r, func(b *model.Board, pid string) error {
		a, ok, err := b.BeginJira(actionID, pid, *in.Revision, c.Target(), label, c.SearchURL(label))
		if err != nil {
			return err
		}
		if !ok {
			return errMutationReplayed
		}
		started = true
		input = jira.Input{Text: a.Text, Assignee: a.Assignee, BoardID: b.ID, BoardTitle: b.Title, Label: label}
		return nil
	})
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if !started {
		writeJSON(w, http.StatusOK, s.view(b, pid))
		return
	}
	// Закрытие вкладки не отменяет сохранение результата. Дедлайн ограничивает
	// запрос к Jira; после аварийного рестарта остаётся безопасная резервация.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 8*time.Second)
	defer cancel()
	issue, failure := c.Create(ctx, input)
	status, message := "created", ""
	if failure != nil {
		status, message = "failed", failure.Message
		if failure.Uncertain {
			status = "unknown"
		}
	}
	s.mutate(w, r, func(b *model.Board, _ string) error {
		changed, err := b.FinishJira(actionID, label, status, issue.Key, issue.URL, message)
		if err == nil && !changed {
			return errMutationReplayed
		}
		return err
	})
}

func (s *Server) handleCheckJira(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	if !s.jiraSlot(w) {
		return
	}
	defer func() { <-s.jiraSlots }()
	pid := participant(r)
	b, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	c := s.options.Jira
	actionID := r.PathValue("actionId")
	link, err := b.JiraForCheck(actionID, pid, c.Target())
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if link.Status == "created" {
		writeJSON(w, http.StatusOK, s.view(b, pid))
		return
	}
	issue, err := c.Find(r.Context(), link.Label)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.mutate(w, r, func(b *model.Board, pid string) error {
		if _, err := b.JiraForCheck(actionID, pid, c.Target()); err != nil {
			return err
		}
		changed, err := b.FinishJira(actionID, link.Label, "created", issue.Key, issue.URL, "")
		if err == nil && !changed {
			return errMutationReplayed
		}
		return err
	})
}
