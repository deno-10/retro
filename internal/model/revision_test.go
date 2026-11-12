package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEditsRejectStaleEntityRevisions(t *testing.T) {
	b := newTestBoard(t)
	c, err := b.AddCard(b.Columns[0].ID, "Исходная карточка", "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	cardID, revision := c.ID, c.Revision
	if b.Revision != 1 || revision != 1 {
		t.Fatalf("новая доска и карточка должны начинаться с версии 1: %d, %d", b.Revision, revision)
	}
	if err := b.EditCard(cardID, "Правка ведущего", "owner-1", revision); err != nil {
		t.Fatal(err)
	}
	if err := b.EditCard(cardID, "Запоздавшая правка автора", "alice", revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("устаревшая правка карточки должна вызвать конфликт: %v", err)
	}
	card := b.View("alice").Columns[0].Cards[0]
	if card.Text != "Правка ведущего" || card.Revision != 2 {
		t.Fatalf("устаревший запрос изменил карточку или её версию: %+v", card)
	}
	if err := b.EditCard(cardID, "Чужая правка", "bob", revision); !errors.Is(err, ErrForbidden) {
		t.Fatalf("права проверяются до версии: %v", err)
	}

	b.Phase = PhaseReveal
	a, err := b.AddAction("Исходное решение", "Анна", "alice")
	if err != nil {
		t.Fatal(err)
	}
	actionID, revision := a.ID, a.Revision
	if revision != 1 {
		t.Fatalf("новое решение должно начинаться с версии 1: %d", revision)
	}
	// Ведущий меняет ответственного, пока автор редактирует старый снимок.
	if err := b.EditAction(actionID, "Исходное решение", "Борис", "owner-1", revision); err != nil {
		t.Fatal(err)
	}
	if err := b.EditAction(actionID, "Уточнение автора", "Анна", "alice", revision); !errors.Is(err, ErrConflict) {
		t.Fatalf("устаревшая правка решения должна вызвать конфликт: %v", err)
	}
	action := b.View("alice").Actions[0]
	if action.Text != "Исходное решение" || action.Assignee != "Борис" || action.Revision != 2 {
		t.Fatalf("конфликтная правка затёрла новое решение: %+v", action)
	}
	if err := b.EditAction(actionID, " ", "Борис", "alice", action.Revision); !errors.Is(err, ErrInvalid) {
		t.Fatalf("пустой текст должен быть отклонён: %v", err)
	}
	if got := b.View("alice").Actions[0].Revision; got != action.Revision {
		t.Fatalf("отклонённая правка изменила версию: %d", got)
	}
}

func TestSetActionDoneIsIdempotent(t *testing.T) {
	b := newTestBoard(t)
	b.Phase = PhaseDiscuss
	a, err := b.AddAction("Сделать", "Анна", "alice")
	if err != nil {
		t.Fatal(err)
	}
	id := a.ID
	for _, actor := range []string{"alice", "owner-1"} {
		if done, err := b.SetActionDone(id, true, actor); err != nil || !done {
			t.Fatalf("повтор команды завершения должен сохранить true: done=%v err=%v", done, err)
		}
		if got := b.View("alice").Actions[0].Revision; got != 2 {
			t.Fatalf("повтор команды не должен менять версию: %d", got)
		}
	}
	if err := b.EditAction(id, "Правка старого снимка", "Анна", "alice", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("завершение должно делать старую правку конфликтной: %v", err)
	}
	for _, actor := range []string{"owner-1", "alice"} {
		if done, err := b.SetActionDone(id, false, actor); err != nil || done {
			t.Fatalf("повтор команды открытия должен сохранить false: done=%v err=%v", done, err)
		}
		if got := b.View("alice").Actions[0].Revision; got != 3 {
			t.Fatalf("версия должна измениться только при смене состояния: %d", got)
		}
	}
}

func TestLegacyZeroRevisionsAndPrivateReceipts(t *testing.T) {
	var b Board
	if err := json.Unmarshal([]byte(`{
		"id":"legacy", "ownerId":"owner-1", "phase":"reveal", "voteLimit":5,
		"columns":[{"id":"col","title":"Колонка","cards":[{"id":"card","text":"До","authorId":"alice"}]}],
		"actions":[{"id":"action","text":"До","assignee":"Анна","authorId":"alice"}]
	}`), &b); err != nil {
		t.Fatal(err)
	}
	v := b.View("alice")
	if v.Revision != 0 || v.Columns[0].Cards[0].Revision != 0 || v.Actions[0].Revision != 0 {
		t.Fatalf("прежние записи без версии должны читаться как версия 0: %+v", v)
	}
	if err := b.EditCard("card", "После", "alice", 0); err != nil {
		t.Fatal(err)
	}
	if err := b.EditAction("action", "После", "Борис", "alice", 0); err != nil {
		t.Fatal(err)
	}
	v = b.View("alice")
	if v.Columns[0].Cards[0].Revision != 1 || v.Actions[0].Revision != 1 {
		t.Fatalf("первая правка прежней записи должна дать версию 1: %+v", v)
	}
	b.Receipts = map[string]MutationReceipt{"private-key": {Operation: "add-card", Fingerprint: "private-fingerprint"}}
	data, err := json.Marshal(b.View("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "receipts") || strings.Contains(string(data), "private-") {
		t.Fatalf("квитанции мутаций не должны попадать в представление: %s", data)
	}
}

func TestNextBoardStartsNewRevisionsWithoutReceipts(t *testing.T) {
	b := newTestBoard(t)
	b.Phase = PhaseDiscuss
	a, err := b.AddAction("Открытое решение", "Анна", "alice")
	if err != nil {
		t.Fatal(err)
	}
	a.Revision = 9
	b.Revision = 20
	b.Receipts = map[string]MutationReceipt{"key": {Operation: "add-action", Fingerprint: "fingerprint"}}
	next, err := b.NextBoard("Следующее", "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision != 1 || next.Actions[0].Revision != 1 || len(next.Receipts) != 0 {
		t.Fatalf("новые сущности должны иметь свои начальные версии и квитанции: %+v", next)
	}
}
