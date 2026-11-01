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
	if md := b.Markdown(); !containsAll(md, "## Что понравилось", "- вторая (2 голоса)", "- первая (1 голос)") {
		t.Fatalf("неожиданный markdown:\n%s", md)
	}
}

func TestNextBoard(t *testing.T) {
	b := newTestBoard(t)
	if err := b.SetPhase(PhaseDiscuss, "owner-1"); err != nil {
		t.Fatal(err)
	}
	open, err := b.AddAction("Настроить алерты", "Боб", "alice")
	if err != nil {
		t.Fatal(err)
	}
	openID := open.ID
	closed, err := b.AddAction("Уже сделано", "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.ToggleActionDone(closed.ID, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetVoteLimit(7, "owner-1"); err != nil {
		t.Fatal(err)
	}

	if _, err := b.NextBoard("", "alice"); err != ErrForbidden {
		t.Fatalf("следующее ретро создаёт только ведущий: %v", err)
	}
	next, err := b.NextBoard("", "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	if next.Title != b.Title || next.Template != b.Template || next.OwnerID != b.OwnerID || next.VoteLimit != 7 {
		t.Fatalf("следующее ретро должно наследовать название, шаблон, ведущего и лимит: %+v", next)
	}
	if next.PrevID != b.ID || next.Phase != PhaseWrite {
		t.Fatalf("ссылка на предыдущее и фаза записи: %+v", next)
	}
	if len(next.Actions) != 1 || next.Actions[0].Text != "Настроить алерты" || next.Actions[0].Assignee != "Боб" {
		t.Fatalf("переноситься должно только незакрытое решение: %+v", next.Actions)
	}
	carried := next.Actions[0]
	if carried.CarriedFrom != b.ID || carried.ID == openID || carried.Done || carried.AuthorID != "alice" {
		t.Fatalf("перенесённое решение: новый id, пометка откуда, автор сохранён: %+v", carried)
	}
	if v := next.View("alice"); !v.Actions[0].Carried || !v.Actions[0].Mine || v.PrevID != b.ID {
		t.Fatalf("представление следующего ретро: %+v", v)
	}
	if md := next.Markdown(); !containsAll(md, "- [ ] Настроить алерты — Боб (перенесено)") {
		t.Fatalf("пометка о переносе в markdown:\n%s", md)
	}

	b.NextID = next.ID
	if _, err := b.NextBoard("ещё одно", "owner-1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("второе следующее ретро создать нельзя: %v", err)
	}
	if v := b.View("bob"); v.NextID != next.ID {
		t.Fatalf("ссылка на следующее в представлении: %+v", v)
	}
	if titled, err := b.NextBoard("Спринт 2", "owner-1"); err == nil || titled != nil {
		t.Fatal("после установки NextID создание должно падать")
	}
}

func TestGrouping(t *testing.T) {
	b := newTestBoard(t)
	col := b.Columns[0].ID
	var ids []string
	for _, text := range []string{"a", "b", "c", "d"} {
		c, err := b.AddCard(col, text, "", "alice")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, c.ID)
	}
	if err := b.GroupCards(ids[0], ids[1], "owner-1"); err != ErrPhase {
		t.Fatalf("в фазе записи группировать нельзя: %v", err)
	}
	if err := b.SetPhase(PhaseReveal, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := b.GroupCards(ids[0], ids[1], "alice"); err != ErrForbidden {
		t.Fatalf("группирует только ведущий: %v", err)
	}
	if err := b.GroupCards(ids[0], ids[0], "owner-1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("карточка с самой собой: %v", err)
	}
	other, _ := b.AddCard(b.Columns[1].ID, "в другой колонке", "", "alice")
	if err := b.GroupCards(ids[0], other.ID, "owner-1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("между колонками группировать нельзя: %v", err)
	}

	// a → c: новая группа из a и c; затем b → в неё же.
	if err := b.GroupCards(ids[0], ids[2], "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := b.GroupCards(ids[1], ids[0], "owner-1"); err != nil {
		t.Fatal(err)
	}
	cards := b.View("bob").Columns[0].Cards
	gid := cards[0].GroupID
	if gid == "" || cards[1].GroupID != gid || cards[2].GroupID != gid || cards[3].GroupID != "" {
		t.Fatalf("группа должна идти подряд на месте первой карточки: %+v", cards)
	}
	if cards[0].ID != ids[0] || cards[1].ID != ids[1] || cards[2].ID != ids[2] {
		t.Fatalf("порядок внутри группы должен сохраняться: %+v", cards)
	}

	// Голос за группу один: ставится на одну карточку, снимается с любой.
	if err := b.SetPhase(PhaseVote, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ToggleVote(ids[2], "bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ToggleVote(ids[3], "carol"); err != nil {
		t.Fatal(err)
	}
	v := b.View("bob").Columns[0].Cards
	if v[0].GroupVotes != 1 || !v[0].GroupVoted || v[2].Votes != 1 {
		t.Fatalf("голоса группы должны суммироваться: %+v", v)
	}
	if voted, err := b.ToggleVote(ids[0], "bob"); err != nil || voted {
		t.Fatalf("повторный голос за группу через другую карточку должен снимать: voted=%v err=%v", voted, err)
	}
	if b.View("bob").Columns[0].Cards[0].GroupVotes != 0 {
		t.Fatal("голос должен быть снят с группы")
	}

	// В обсуждении одиночная d с одним голосом идёт раньше группы без голосов.
	if err := b.SetPhase(PhaseDiscuss, "owner-1"); err != nil {
		t.Fatal(err)
	}
	v = b.View("bob").Columns[0].Cards
	if v[0].ID != ids[3] || v[1].GroupID != gid || v[3].GroupID != gid {
		t.Fatalf("сортировка по голосам должна держать группу вместе: %+v", v)
	}
	if md := b.Markdown(); !containsAll(md, "- d (1 голос)", "- a\n  - b\n  - c") {
		t.Fatalf("группа в markdown — вложенный список:\n%s", md)
	}

	// Выход из группы и роспуск группы из одной карточки.
	if err := b.UngroupCard(ids[1], "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteCard(ids[0], "owner-1"); err != nil {
		t.Fatal(err)
	}
	for _, c := range b.Columns[0].Cards {
		if c.GroupID != "" {
			t.Fatalf("после удаления в группе осталась одна карточка — группа должна распасться: %+v", c)
		}
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

func TestExpireTimerAdvancesPhase(t *testing.T) {
	b := newTestBoard(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if b.ExpireTimer(now) {
		t.Fatal("без таймера истекать нечему")
	}
	if err := b.SetTimer(5, now, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if b.ExpireTimer(now.Add(4*time.Minute)) || b.Phase != PhaseWrite || b.TimerEndsAt == nil {
		t.Fatal("до конца таймера ничего не должно меняться")
	}
	if !b.ExpireTimer(now.Add(5*time.Minute)) || b.Phase != PhaseReveal || b.TimerEndsAt != nil {
		t.Fatalf("по истечении запись должна смениться показом и таймер сняться: phase=%s ends=%v", b.Phase, b.TimerEndsAt)
	}
	if b.ExpireTimer(now.Add(time.Hour)) {
		t.Fatal("повторное применение не должно ничего менять")
	}

	if err := b.SetPhase(PhaseDiscuss, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := b.SetTimer(1, now, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if !b.ExpireTimer(now.Add(time.Minute)) || b.Phase != PhaseDiscuss || b.TimerEndsAt != nil {
		t.Fatalf("на последнем этапе таймер снимается, фаза остаётся: phase=%s ends=%v", b.Phase, b.TimerEndsAt)
	}

	if next, ok := PhaseVote.Next(); !ok || next != PhaseDiscuss {
		t.Fatalf("после голосования идёт обсуждение, получено %s %v", next, ok)
	}
	if _, ok := PhaseDiscuss.Next(); ok {
		t.Fatal("у обсуждения нет следующей фазы")
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
