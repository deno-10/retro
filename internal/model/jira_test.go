package model

import (
	"errors"
	"testing"
)

func TestJiraReservationAndNextBoard(t *testing.T) {
	b := newTestBoard(t)
	b.Phase = PhaseDiscuss
	a, err := b.AddAction("Настроить алерты", "Анна", "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	id, revision := a.ID, a.Revision
	_, started, err := b.BeginJira(id, "owner-1", revision, "target", "label", "https://jira.example.org/issues")
	if err != nil || !started {
		t.Fatal("не удалось зарезервировать отправку", err)
	}
	if _, started, err = b.BeginJira(id, "owner-1", revision, "target", "other", ""); err != nil || started {
		t.Fatal("повтор создал новую отправку")
	}
	if err := b.EditAction(id, "Правка", "", "owner-1", a.Revision); !errors.Is(err, ErrConflict) {
		t.Fatal("незавершённую отправку нельзя редактировать")
	}
	if err := b.DeleteAction(id, "owner-1"); !errors.Is(err, ErrConflict) {
		t.Fatal("потеряна незавершённая отправка")
	}
	if _, err := b.NextBoard("Следующее", "owner-1"); !errors.Is(err, ErrConflict) {
		t.Fatal("следующее ретро потеряло незавершённую отправку")
	}
	if _, err := b.FinishJira(id, "label", "created", "TEAM-7", "https://jira.example.org/browse/TEAM-7", ""); err != nil {
		t.Fatal(err)
	}
	next, err := b.NextBoard("Следующее", "owner-1")
	if err != nil {
		t.Fatal(err)
	}
	if next.Actions[0].Jira.Key != "TEAM-7" || next.Actions[0].Jira == b.Actions[0].Jira {
		t.Fatal("ссылка не перенесена отдельной копией")
	}
	if _, changed, err := next.BeginJira(next.Actions[0].ID, "owner-1", 1, "target", "new", ""); err != nil || changed {
		t.Fatal("перенос создал дубликат")
	}
	view := next.View("участник")
	view.Actions[0].Jira.Key = "изменение представления"
	if next.Actions[0].Jira.Key != "TEAM-7" {
		t.Fatal("представление изменило исходную доску")
	}
}

func TestJiraPermissionsVersionsAndOldRetro(t *testing.T) {
	b := newTestBoard(t)
	b.Phase = PhaseReveal
	a, _ := b.AddAction("Решение", "", "author")
	if _, _, err := b.BeginJira(a.ID, "author", a.Revision, "target", "label", ""); !errors.Is(err, ErrForbidden) {
		t.Fatal("участник получил доступ к сервисной Jira")
	}
	if _, _, err := b.BeginJira(a.ID, "owner-1", a.Revision, "target", "label", ""); !errors.Is(err, ErrPhase) {
		t.Fatal("экспорт до обсуждения")
	}
	b.Phase = PhaseDiscuss
	if _, _, err := b.BeginJira(a.ID, "owner-1", 0, "target", "label", ""); !errors.Is(err, ErrConflict) {
		t.Fatal("отправлена устаревшая версия")
	}
	b.NextID = "next"
	if _, _, err := b.BeginJira(a.ID, "owner-1", a.Revision, "target", "label", ""); !errors.Is(err, ErrConflict) {
		t.Fatal("старое ретро создаёт дубликат перенесённого решения")
	}
}
