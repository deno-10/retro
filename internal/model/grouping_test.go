package model

import (
	"errors"
	"reflect"
	"testing"
)

func TestSuggestGroupsWithinColumns(t *testing.T) {
	b := Board{OwnerID: "owner", Phase: PhaseReveal, Columns: []Column{
		{ID: "good", Cards: []Card{
			{ID: "a", Text: "Слишком долгие созвоны!"},
			{ID: "separate", Text: "Тесты падают на CI"},
			{ID: "b", Text: "Долгие встречи"},
			{ID: "c", Text: "Встречи долго идут"},
			{ID: "negation", Text: "Встречи НЕ долгие"},
			{ID: "manual", Text: "Долгие созвоны", GroupID: "existing"},
			{ID: "manual2", Text: "Долгие встречи", GroupID: "existing"},
			{ID: "noise1", Text: "и на это"}, {ID: "noise2", Text: "а это"},
		}},
		{ID: "bad", Cards: []Card{
			{ID: "unmatched", Text: "Долгие созвоны"},
			{ID: "d", Text: "Ждём кодревью"}, {ID: "e", Text: "Ждали ревью"},
		}},
	}}
	want := []ColumnGroups{{ColumnID: "good", Groups: [][]string{{"a", "b", "c"}}},
		{ColumnID: "bad", Groups: [][]string{{"d", "e"}}}}
	for range 2 {
		got, err := b.SuggestGroups("owner")
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("ожидались %v, получены %v, ошибка %v", want, got, err)
		}
	}
	if b.Columns[0].Cards[0].GroupID != "" || b.Columns[0].Cards[5].GroupID != "existing" {
		t.Fatal("предварительный просмотр изменил доску")
	}
}

func TestGroupSuggestionsAvoidTransitiveChains(t *testing.T) {
	col := Column{Cards: []Card{{ID: "a", Text: "сборка пайплайн"}, {ID: "b", Text: "сборка CI тесты"},
		{ID: "c", Text: "пайплайн тестирование"}, {ID: "d", Text: "планирование"}, {ID: "e", Text: "тест"}}}
	if got := suggestColumn(col); !reflect.DeepEqual(got, [][]string{{"a", "b"}}) {
		t.Fatalf("карточка-посредник или одно общее слово объединили разные темы: %v", got)
	}
}

func TestApplyGroupsValidatesBeforeChangingBoard(t *testing.T) {
	b := Board{OwnerID: "owner", Phase: PhaseReveal, Revision: 10, Columns: []Column{
		{ID: "one", Cards: []Card{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "manual1", GroupID: "manual"}, {ID: "manual2", GroupID: "manual"}}},
		{ID: "two", Cards: []Card{{ID: "x"}, {ID: "y"}}},
	}}
	first := ColumnGroups{ColumnID: "one", Groups: [][]string{{"a", "b"}}}
	for _, tc := range []struct {
		name string
		plan []ColumnGroups
		want error
	}{
		{"пустой план", nil, ErrInvalid},
		{"пустая колонка", []ColumnGroups{{ColumnID: "one"}}, ErrInvalid},
		{"одна карточка", []ColumnGroups{first, {ColumnID: "two", Groups: [][]string{{"x"}}}}, ErrInvalid},
		{"другая колонка", []ColumnGroups{first, {ColumnID: "two", Groups: [][]string{{"x", "c"}}}}, ErrInvalid},
		{"повтор колонки", []ColumnGroups{first, first}, ErrInvalid},
		{"повтор карточки", []ColumnGroups{{ColumnID: "one", Groups: [][]string{{"a", "b"}, {"c", "a"}}}}, ErrInvalid},
		{"нет карточки", []ColumnGroups{first, {ColumnID: "two", Groups: [][]string{{"x", "missing"}}}}, ErrInvalid},
		{"ручная группа", []ColumnGroups{{ColumnID: "one", Groups: [][]string{{"a", "b"}, {"c", "manual1"}}}}, ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := b.ApplyGroups("owner", 10, tc.plan); !errors.Is(err, tc.want) {
				t.Fatalf("ожидалась ошибка %v, получено %v", tc.want, err)
			}
			if b.Columns[0].Cards[0].GroupID != "" || b.Columns[0].Cards[3].GroupID != "manual" {
				t.Fatal("ошибка оставила часть групп на доске")
			}
		})
	}
	second := ColumnGroups{ColumnID: "two", Groups: [][]string{{"y", "x"}}}
	if err := b.ApplyGroups("owner", 10, []ColumnGroups{first, second}); err != nil {
		t.Fatal(err)
	}
	cards, other := b.Columns[0].Cards, b.Columns[1].Cards
	if cards[0].GroupID == "" || cards[0].GroupID != cards[1].GroupID || cards[2].GroupID != "" ||
		other[0].GroupID == "" || other[0].GroupID != other[1].GroupID || other[0].GroupID == cards[0].GroupID ||
		cards[3].GroupID != "manual" || cards[4].GroupID != "manual" {
		t.Fatalf("неверный состав: %+v", b.Columns)
	}
}
