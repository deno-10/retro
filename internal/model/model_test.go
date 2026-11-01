package model

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func newTestBoard(t *testing.T) *Board {
	t.Helper()
	b, err := NewBoard("Спринт 1", "classic", "owner-1")
	if err != nil {
		t.Fatalf("NewBoard: %v", err)
	}
	return b
}

func TestNewBoardTemplate(t *testing.T) {
	b := newTestBoard(t)
	if len(b.Columns) != 3 {
		t.Fatalf("ожидалось 3 колонки, получено %d", len(b.Columns))
	}
	if b.Phase != PhaseWrite {
		t.Fatalf("новая доска должна быть в фазе write, получено %s", b.Phase)
	}
	if _, err := NewBoard("x", "nope", "owner-1"); err == nil {
		t.Fatal("неизвестный шаблон должен давать ошибку")
	}
}

func TestWritePhaseHidesOthersCards(t *testing.T) {
	b := newTestBoard(t)
	col := b.Columns[0].ID
	if _, err := b.AddCard(col, "моя", "A", "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddCard(col, "чужая", "B", "bob"); err != nil {
		t.Fatal(err)
	}
	cards := b.View("alice").Columns[0].Cards
	if len(cards) != 2 {
		t.Fatalf("ожидалось 2 карточки, получено %d", len(cards))
	}
	if cards[0].Hidden || cards[0].Text != "моя" || !cards[0].Mine {
		t.Errorf("своя карточка должна быть видна: %+v", cards[0])
	}
	if !cards[1].Hidden || cards[1].Text != "" || cards[1].Author != "" {
		t.Errorf("чужая карточка должна быть скрыта: %+v", cards[1])
	}
	if err := b.SetPhase(PhaseReveal, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if c := b.View("alice").Columns[0].Cards[1]; c.Hidden || c.Text != "чужая" {
		t.Errorf("после показа карточка должна быть видна: %+v", c)
	}
}

func TestVoteLimit(t *testing.T) {
	b := newTestBoard(t)
	col := b.Columns[0].ID
	var ids []string
	for i := 0; i < 3; i++ {
		c, err := b.AddCard(col, "карточка", "", "alice")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	if _, err := b.ToggleVote(ids[0], "bob"); err != ErrPhase {
		t.Fatalf("голосование до фазы vote должно давать ErrPhase, получено %v", err)
	}
	if err := b.SetVoteLimit(2, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPhase(PhaseVote, "owner-1"); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[:2] {
		if voted, err := b.ToggleVote(id, "bob"); err != nil || !voted {
			t.Fatalf("голос должен ставиться: voted=%v err=%v", voted, err)
		}
	}
	if _, err := b.ToggleVote(ids[2], "bob"); err != ErrVotes {
		t.Fatalf("третий голос должен упереться в лимит, получено %v", err)
	}
	if voted, err := b.ToggleVote(ids[0], "bob"); err != nil || voted {
		t.Fatalf("повторный клик должен снимать голос: voted=%v err=%v", voted, err)
	}
	if left := b.View("bob").VotesLeft; left != 1 {
		t.Fatalf("ожидался 1 оставшийся голос, получено %d", left)
	}
}

func TestOnlyOwnerChangesPhase(t *testing.T) {
	b := newTestBoard(t)
	if err := b.SetPhase(PhaseReveal, "bob"); err != ErrForbidden {
		t.Fatalf("ожидался ErrForbidden, получено %v", err)
	}
	if err := b.SetPhase("nope", "owner-1"); err == nil {
		t.Fatal("неизвестная фаза должна давать ошибку")
	}
}

func TestEditPermissions(t *testing.T) {
	b := newTestBoard(t)
	col := b.Columns[0].ID
	c, err := b.AddCard(col, "текст", "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	id := c.ID
	if err := b.EditCard(id, "новый", "bob"); err != ErrForbidden {
		t.Fatalf("чужую карточку править нельзя: %v", err)
	}
	if err := b.EditCard(id, "новый", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteCard(id, "owner-1"); err != nil {
		t.Fatalf("ведущий может удалить любую карточку: %v", err)
	}
	if len(b.Columns[0].Cards) != 0 {
		t.Fatal("карточка должна быть удалена")
	}
}

func TestDiscussSortsByVotes(t *testing.T) {
	b := newTestBoard(t)
	col := b.Columns[0].ID
	first, err := b.AddCard(col, "первая", "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	firstID := first.ID
	second, err := b.AddCard(col, "вторая", "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	secondID := second.ID
	if err := b.SetPhase(PhaseVote, "owner-1"); err != nil {
		t.Fatal(err)
	}
	for _, v := range []struct{ card, who string }{{secondID, "bob"}, {secondID, "carol"}, {firstID, "bob"}} {
		if _, err := b.ToggleVote(v.card, v.who); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.SetPhase(PhaseDiscuss, "owner-1"); err != nil {
		t.Fatal(err)
	}
	cards := b.View("alice").Columns[0].Cards
	if cards[0].ID != secondID || cards[0].Votes != 2 {
		t.Fatalf("первой должна идти карточка с 2 голосами: %+v", cards)
	}
	if md := b.Markdown(); !containsAll(md, "## Что было хорошо", "- вторая (2 голоса)", "- первая (1 голос)") {
		t.Fatalf("неожиданный markdown:\n%s", md)
	}
}

func TestActions(t *testing.T) {
	b := newTestBoard(t)
	if _, err := b.AddAction("рано", "", "alice"); err != ErrPhase {
		t.Fatalf("в фазе записи решения недоступны, получено %v", err)
	}
	if err := b.SetPhase(PhaseDiscuss, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AddAction("   ", "", "alice"); err == nil {
		t.Fatal("пустое решение должно давать ошибку")
	}
	a, err := b.AddAction("Настроить алерты", " Боб ", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if a.Assignee != "Боб" {
		t.Fatalf("ответственный должен быть обрезан: %q", a.Assignee)
	}
	id := a.ID

	if err := b.EditAction(id, "x", "", "bob"); err != ErrForbidden {
		t.Fatalf("чужое решение править нельзя: %v", err)
	}
	if _, err := b.ToggleActionDone(id, "bob"); err != ErrForbidden {
		t.Fatalf("чужое решение отмечать нельзя: %v", err)
	}
	if err := b.EditAction(id, "Настроить алерты в проде", "Боб", "alice"); err != nil {
		t.Fatal(err)
	}
	if done, err := b.ToggleActionDone(id, "owner-1"); err != nil || !done {
		t.Fatalf("ведущий может отметить сделанным: done=%v err=%v", done, err)
	}
	if done, err := b.ToggleActionDone(id, "alice"); err != nil || done {
		t.Fatalf("повторная отметка снимает: done=%v err=%v", done, err)
	}

	v := b.View("alice")
	if len(v.Actions) != 1 || !v.Actions[0].Mine || v.Actions[0].Text != "Настроить алерты в проде" {
		t.Fatalf("неожиданное представление решений: %+v", v.Actions)
	}
	if b.View("bob").Actions[0].Mine {
		t.Fatal("чужое решение не должно помечаться как своё")
	}

	if _, err := b.ToggleActionDone(id, "alice"); err != nil {
		t.Fatal(err)
	}
	if md := b.Markdown(); !containsAll(md, "## Решения", "- [x] Настроить алерты в проде — Боб") {
		t.Fatalf("решения должны попадать в markdown:\n%s", md)
	}

	if err := b.DeleteAction(id, "bob"); err != ErrForbidden {
		t.Fatalf("чужое решение удалять нельзя: %v", err)
	}
	if err := b.DeleteAction(id, "owner-1"); err != nil || len(b.Actions) != 0 {
		t.Fatalf("ведущий удаляет любое решение: err=%v n=%d", err, len(b.Actions))
	}
	if err := b.DeleteAction("nope", "owner-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ожидался ErrNotFound, получено %v", err)
	}
}

func TestTimer(t *testing.T) {
	b := newTestBoard(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if err := b.SetTimer(5, now, "bob"); err != ErrForbidden {
		t.Fatalf("таймер ставит только ведущий, получено %v", err)
	}
	if err := b.SetTimer(MaxTimerMinutes+1, now, "owner-1"); err == nil {
		t.Fatal("слишком долгий таймер должен давать ошибку")
	}
	if err := b.SetTimer(-1, now, "owner-1"); err == nil {
		t.Fatal("отрицательный таймер должен давать ошибку")
	}
	if err := b.SetTimer(5, now, "owner-1"); err != nil {
		t.Fatal(err)
	}
	want := now.Add(5 * time.Minute)
	if b.TimerEndsAt == nil || !b.TimerEndsAt.Equal(want) {
		t.Fatalf("ожидался конец таймера %v, получено %v", want, b.TimerEndsAt)
	}
	if v := b.View("bob"); v.TimerEndsAt == nil || !v.TimerEndsAt.Equal(want) {
		t.Fatalf("таймер должен быть виден всем участникам: %+v", v.TimerEndsAt)
	}
	if err := b.SetTimer(0, now, "owner-1"); err != nil || b.TimerEndsAt != nil {
		t.Fatalf("ноль минут должен останавливать таймер: err=%v ends=%v", err, b.TimerEndsAt)
	}
	if err := b.SetTimer(3, now, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPhase(PhaseWrite, "owner-1"); err != nil || b.TimerEndsAt == nil {
		t.Fatalf("повторный выбор той же фазы не должен сбрасывать таймер: err=%v", err)
	}
	if err := b.SetPhase(PhaseReveal, "owner-1"); err != nil || b.TimerEndsAt != nil {
		t.Fatalf("смена фазы должна сбрасывать таймер: err=%v ends=%v", err, b.TimerEndsAt)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
