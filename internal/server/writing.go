package server

import (
	"fmt"
	"net/http"

	"retro/internal/model"
)

func (s *Server) handleJoinWriting(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Round *uint64 `json:"round"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	if in.Round == nil {
		s.writeErr(w, fmt.Errorf("%w: укажите round — номер этапа записи", model.ErrInvalid))
		return
	}
	s.mutate(w, r, func(b *model.Board, pid string) error {
		changed, err := b.JoinWriting(pid, *in.Round)
		if err == nil && !changed {
			return errMutationReplayed
		}
		return err
	})
}

func (s *Server) handleWritingReady(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ready    *bool   `json:"ready"`
		Round    *uint64 `json:"round"`
		Revision *uint64 `json:"revision"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	if in.Ready == nil || in.Round == nil || in.Revision == nil {
		s.writeErr(w, fmt.Errorf("%w: укажите ready, round и revision", model.ErrInvalid))
		return
	}
	s.mutate(w, r, func(b *model.Board, pid string) error {
		changed, err := b.SetWritingReady(pid, *in.Ready, *in.Round, *in.Revision)
		if err == nil && !changed {
			return errMutationReplayed
		}
		return err
	})
}
