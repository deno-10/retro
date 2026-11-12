package model

import (
	"errors"
	"testing"
)

func voteTestCards(t *testing.T, b *Board, count int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for range count {
		card, err := b.AddCard(b.Columns[0].ID, "Карточка", "", "alice")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, card.ID)
	}
	return ids
}

func TestSetVoteRepeatedCommandsKeepIntendedState(t *testing.T) {
	b := newTestBoard(t)
	ids := voteTestCards(t, b, 2)
	if err := b.SetVoteLimit(1, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPhase(PhaseVote, "owner-1"); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if voted, err := b.SetVote(ids[0], "bob", true); err != nil || !voted {
			t.Fatalf("повтор установки голоса при исчерпанном лимите: voted=%v err=%v", voted, err)
		}
		if b.VotesUsed("bob") != 1 || b.View("bob").Columns[0].Cards[0].Votes != 1 {
			t.Fatal("повтор команды снял или продублировал голос")
		}
	}
	if _, err := b.SetVote(ids[1], "bob", true); !errors.Is(err, ErrVotes) {
		t.Fatalf("новый голос всё ещё должен соблюдать лимит: %v", err)
	}
	// Снятие отсутствующего голоса не создаёт голос даже при исчерпанном лимите.
	if voted, err := b.SetVote(ids[1], "bob", false); err != nil || voted || b.VotesUsed("bob") != 1 {
		t.Fatalf("снятие отсутствующего голоса изменило бюджет: voted=%v err=%v", voted, err)
	}
	for range 3 {
		if voted, err := b.SetVote(ids[0], "bob", false); err != nil || voted {
			t.Fatalf("повтор снятия голоса: voted=%v err=%v", voted, err)
		}
		if b.VotesUsed("bob") != 0 || b.View("bob").Columns[0].Cards[0].Voted {
			t.Fatal("повтор снятия снова поставил голос")
		}
	}
	if voted, err := b.SetVote(ids[1], "bob", true); err != nil || !voted {
		t.Fatalf("снятый голос должен освободить бюджет: voted=%v err=%v", voted, err)
	}
}

func TestSetVoteGroupRemovesAllMergedParticipantVotes(t *testing.T) {
	b := newTestBoard(t)
	ids := voteTestCards(t, b, 3)
	if err := b.SetVoteLimit(3, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPhase(PhaseVote, "owner-1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := b.SetVote(id, "bob", true); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids[:2] {
		if _, err := b.SetVote(id, "carol", true); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.GroupCards(ids[0], ids[1], "owner-1"); err != nil {
		t.Fatal(err)
	}
	// Суммирование старых голосов при объединении предусмотрено моделью.
	if votes := b.View("bob").Columns[0].Cards[0].GroupVotes; votes != 4 {
		t.Fatalf("после объединения ожидались 4 голоса, получено %d", votes)
	}
	for _, id := range ids[:2] {
		if voted, err := b.SetVote(id, "bob", true); err != nil || !voted || b.VotesUsed("bob") != 3 {
			t.Fatalf("повтор через участника группы не должен расходовать голос: voted=%v err=%v", voted, err)
		}
	}
	for _, id := range ids[:2] {
		if voted, err := b.SetVote(id, "bob", false); err != nil || voted {
			t.Fatalf("снятие с группы: voted=%v err=%v", voted, err)
		}
		view := b.View("bob").Columns[0].Cards
		if b.VotesUsed("bob") != 1 || !view[2].Voted || view[0].GroupVoted || view[1].GroupVoted {
			t.Fatalf("снятие должно убрать оба голоса группы, оставив одиночный: %+v", view)
		}
		if b.VotesUsed("carol") != 2 || view[0].GroupVotes != 2 {
			t.Fatal("снятие чужого голоса затронуло другого участника")
		}
	}
	// После снятия два одинаковых намерения через разные карточки дают один голос.
	for _, id := range ids[:2] {
		if voted, err := b.SetVote(id, "bob", true); err != nil || !voted {
			t.Fatalf("установка на группу: voted=%v err=%v", voted, err)
		}
	}
	view := b.View("bob").Columns[0].Cards
	if b.VotesUsed("bob") != 2 || view[0].GroupVotes != 3 || !view[0].GroupVoted || !view[1].GroupVoted {
		t.Fatalf("повторные установки должны дать группе один новый голос: %+v", view)
	}
}

func TestSetVoteKeepsPhaseAndMissingCardChecks(t *testing.T) {
	b := newTestBoard(t)
	id := voteTestCards(t, b, 1)[0]
	for _, voted := range []bool{false, true} {
		if _, err := b.SetVote(id, "bob", voted); !errors.Is(err, ErrPhase) {
			t.Fatalf("команда voted=%v до голосования должна быть запрещена: %v", voted, err)
		}
	}
	if err := b.SetPhase(PhaseVote, "owner-1"); err != nil {
		t.Fatal(err)
	}
	for _, voted := range []bool{false, true} {
		if _, err := b.SetVote("missing", "bob", voted); !errors.Is(err, ErrNotFound) {
			t.Fatalf("команда voted=%v для отсутствующей карточки: %v", voted, err)
		}
	}
}
