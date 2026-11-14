package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestWritingAllReadyRevealsAndClearsTimer(t *testing.T) {
	b := newTestBoard(t)
	if _, err := b.JoinWriting("alice", 1); err != nil {
		t.Fatal(err)
	}
	if err := b.SetTimer(5, time.Now(), b.OwnerID); err != nil {
		t.Fatal(err)
	}
	if changed, err := b.SetWritingReady(b.OwnerID, true, 1, 0); err != nil || !changed {
		t.Fatalf("готовность ведущего: %v, %v", changed, err)
	}
	if v := b.View(b.OwnerID).Writing; v.Ready != 1 || v.Participants != 2 || !v.Mine || b.Phase != PhaseWrite || b.TimerEndsAt == nil {
		t.Fatalf("нужно дождаться второго участника: %+v", v)
	}
	if changed, err := b.JoinWriting(b.OwnerID, 1); err != nil || changed {
		t.Fatalf("повторный вход не должен сбрасывать готовность: %v, %v", changed, err)
	}
	if changed, err := b.SetWritingReady(b.OwnerID, true, 1, 0); err != nil || changed {
		t.Fatalf("повтор готовности не должен менять доску: %v, %v", changed, err)
	}
	if changed, err := b.SetWritingReady("alice", true, 1, 0); err != nil || !changed {
		t.Fatalf("последняя готовность: %v, %v", changed, err)
	}
	if b.Phase != PhaseReveal || b.TimerEndsAt != nil || b.View("alice").Writing != nil {
		t.Fatal("все готовы: нужен показ без таймера и панели готовности")
	}
	if changed, err := b.SetWritingReady("alice", true, 1, 0); err != nil || changed || b.Phase != PhaseReveal {
		t.Fatal("повтор последнего нажатия не должен переключать этап ещё раз")
	}
	if b.ExpireTimer(time.Now().Add(time.Hour)) {
		t.Fatal("старый дедлайн не должен переводить показ в голосование")
	}
}

func TestWritingUndoAndOldRound(t *testing.T) {
	b := newTestBoard(t)
	b.JoinWriting("alice", 1)
	b.SetWritingReady(b.OwnerID, true, 1, 0)
	if _, err := b.SetWritingReady(b.OwnerID, false, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetWritingReady(b.OwnerID, true, 1, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("старое нажатие после отмены: %v", err)
	}
	b.SetWritingReady("alice", true, 1, 0)
	if b.Phase != PhaseWrite {
		t.Fatal("ведущий отменил готовность — переходить рано")
	}
	b.SetPhase(PhaseReveal, b.OwnerID)
	b.SetPhase(PhaseWrite, b.OwnerID)
	if v := b.View(b.OwnerID).Writing; v.Round != 2 || v.Ready != 0 || v.Participants != 2 {
		t.Fatalf("новый этап должен сбросить отметки и сохранить состав: %+v", v)
	}
	if _, err := b.SetWritingReady("alice", true, 1, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("запрос прежнего этапа должен быть отклонён: %v", err)
	}
	if _, err := b.JoinWriting("late-user", 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("запоздавший вход не должен менять новый этап: %v", err)
	}
	b.SetPhase(PhaseWrite, b.OwnerID)
	if b.Writing.Round != 2 {
		t.Fatal("повтор той же фазы не должен начинать новый этап")
	}
}

func TestWritingCardChangesCancelReadiness(t *testing.T) {
	for _, operation := range []string{"add", "edit", "delete"} {
		t.Run(operation, func(t *testing.T) {
			b := newTestBoard(t)
			card, err := b.AddCard(b.Columns[0].ID, "Мысль", "", "alice")
			if err != nil {
				t.Fatal(err)
			}
			b.SetWritingReady("alice", true, 1, 1)
			switch operation {
			case "add":
				_, err = b.AddCard(b.Columns[0].ID, "Ещё мысль", "", "alice")
			case "edit":
				err = b.EditCard(card.ID, "Правка", "alice", card.Revision)
			case "delete":
				err = b.DeleteCard(card.ID, "alice")
			}
			if err != nil {
				t.Fatal(err)
			}
			if v := b.View("alice").Writing; v.Mine || v.Ready != 0 || v.Revision != 3 {
				t.Fatalf("правка должна снять готовность и обновить версию: %+v", v)
			}
			if _, err := b.SetWritingReady("alice", true, 1, 1); !errors.Is(err, ErrConflict) {
				t.Fatalf("старое нажатие не должно обгонять новую правку: %v", err)
			}
		})
	}
}

func TestWritingLegacyAndPrivacy(t *testing.T) {
	b := newTestBoard(t)
	b.AddCard(b.Columns[0].ID, "Секрет", "", "legacy-author")
	b.Writing = nil
	v := b.View(b.OwnerID)
	if v.Writing.Round != 1 || v.Writing.Participants != 2 || !v.Writing.Joined || v.Writing.Mine {
		t.Fatalf("старая доска должна учитывать ведущего и авторов карточек: %+v", v.Writing)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "legacy-author") || strings.Contains(string(data), b.OwnerID) {
		t.Fatal("список идентификаторов не должен попадать в ответ API")
	}
	if changed, err := b.JoinWriting("new-user", 1); err != nil || !changed || len(b.Writing.Participants) != 3 {
		t.Fatalf("вход на старую доску: %v, %v", changed, err)
	}
	if _, err := b.SetWritingReady("stranger", true, 1, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("неучтённый участник не должен завершать этап: %v", err)
	}
}

func TestWritingParticipantLimitAndSingleParticipant(t *testing.T) {
	b := newTestBoard(t)
	for i := 1; i < MaxWritingParticipants; i++ {
		b.JoinWriting(fmt.Sprintf("user-%d", i), 1)
	}
	if _, err := b.JoinWriting("too-many", 1); !errors.Is(err, ErrCapacity) {
		t.Fatalf("лимит состава этапа: %v", err)
	}
	b = newTestBoard(t)
	if _, err := b.SetWritingReady(b.OwnerID, true, 1, 0); err != nil || b.Phase != PhaseReveal {
		t.Fatalf("единственный участник тоже может закончить без таймера: %v", err)
	}
}
