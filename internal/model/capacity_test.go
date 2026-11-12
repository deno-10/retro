package model

import (
	"errors"
	"testing"
)

func TestCardAndActionCapacity(t *testing.T) {
	b, _ := NewBoard("Лимиты", "classic", "owner-00000001")
	b.Columns[0].Cards = make([]Card, MaxCards)
	if _, err := b.AddCard(b.Columns[1].ID, "Лишняя", "", b.OwnerID); !errors.Is(err, ErrCapacity) {
		t.Fatalf("лимит должен охватывать все колонки: %v", err)
	}
	b.Columns[0].Cards = b.Columns[0].Cards[1:]
	if _, err := b.AddCard(b.Columns[1].ID, "Помещается", "", b.OwnerID); err != nil {
		t.Fatal(err)
	}
	b.Phase = PhaseDiscuss
	b.Actions = make([]Action, MaxActions)
	if _, err := b.AddAction("Лишнее", "", b.OwnerID); !errors.Is(err, ErrCapacity) {
		t.Fatal(err)
	}
	b.Actions = b.Actions[1:]
	if _, err := b.AddAction("Помещается", "", b.OwnerID); err != nil {
		t.Fatal(err)
	}
}
