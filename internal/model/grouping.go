// Предложения рассчитываются на этапе показа по тексту свободных карточек.
// Черновик ведущего не хранится на доске. Только подтверждение меняет GroupID.
package model

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

type ColumnGroups struct {
	ColumnID string     `json:"columnId"`
	Groups   [][]string `json:"groups"`
}

func (b *Board) CheckGroupReview(actor string) error {
	if actor == "" || actor != b.OwnerID {
		return ErrForbidden
	}
	if b.Phase != PhaseReveal {
		return ErrPhase
	}
	return nil
}

func (b *Board) SuggestGroups(actor string) ([]ColumnGroups, error) {
	if err := b.CheckGroupReview(actor); err != nil {
		return nil, err
	}
	columns := []ColumnGroups{}
	for _, col := range b.Columns {
		if groups := suggestColumn(col); len(groups) > 0 {
			columns = append(columns, ColumnGroups{ColumnID: col.ID, Groups: groups})
		}
	}
	return columns, nil
}

// ApplyGroups проверяет весь просмотренный план до записи первой группы.
// Та же версия доски не может заменить более свежую ручную правку.
func (b *Board) ApplyGroups(actor string, revision uint64, plan []ColumnGroups) error {
	if err := b.CheckGroupReview(actor); err != nil {
		return err
	}
	if b.Revision != revision {
		return fmt.Errorf("%w: доска изменилась, обновите предложения", ErrConflict)
	}
	if len(plan) == 0 || len(plan) > len(b.Columns) {
		return fmt.Errorf("%w: выберите колонки с группами", ErrInvalid)
	}
	seenColumns := make(map[string]bool)
	seenCards := make(map[string]bool)
	var checked [][]*Card
	for _, column := range plan {
		index := slices.IndexFunc(b.Columns, func(c Column) bool { return c.ID == column.ColumnID })
		if index < 0 || seenColumns[column.ColumnID] || len(column.Groups) == 0 {
			return fmt.Errorf("%w: неверная или повторяющаяся колонка", ErrInvalid)
		}
		seenColumns[column.ColumnID] = true
		cards := make(map[string]*Card)
		for i := range b.Columns[index].Cards {
			card := &b.Columns[index].Cards[i]
			cards[card.ID] = card
		}
		for _, ids := range column.Groups {
			if len(ids) < 2 || len(ids) > len(cards) {
				return fmt.Errorf("%w: группа должна содержать хотя бы две карточки одной колонки", ErrInvalid)
			}
			group := make([]*Card, 0, len(ids))
			for _, id := range ids {
				card := cards[id]
				if card == nil || seenCards[id] {
					return fmt.Errorf("%w: проверьте состав групп в колонке", ErrInvalid)
				}
				if card.GroupID != "" {
					return fmt.Errorf("%w: карточка уже входит в группу", ErrConflict)
				}
				seenCards[id] = true
				group = append(group, card)
			}
			checked = append(checked, group)
		}
	}
	for _, group := range checked {
		id := NewID()
		for _, card := range group {
			card.GroupID = id
		}
	}
	return nil
}

// Термины из лексики команд помогают сопоставлять короткие карточки без
// внешней модели. Слова вне словаря тоже участвуют в сравнении.
var groupingTerms = func() map[string]string {
	terms := make(map[string]string)
	for topic, words := range map[string]string{
		"":            "и а но как что чтобы это этот эта эти на по до из за для про от при в во с со к у мы вы я нам нас наш наша наши было были был есть очень слишком хочется хочу нужно надо стоит часто стало стали the a an to of and we our is are was were for with in on too need should",
		"review":      "ревью ревьюить кодревью кодревьюинг ревьюшки review reviews",
		"pullrequest": "pr prs пр mr пулреквест пулреквесты пулреквестов мержреквест",
		"meeting":     "созвон созвона созвоне созвону созвоном созвоны созвонов созвонах встреча встрече встречи встречу встречах встреч митинг митинги митингов meeting meetings",
		"many":        "много множество many numerous",
		"few":         "мало немного few",
		"build":       "сборка сборки сборку сборке сборок билд билды билда билдов build builds",
		"pipeline":    "ci пайплайн пайплайны пайплайна пайплайнов pipeline pipelines",
		"deploy":      "деплой деплоя деплои деплоями деплоить деплоим выкладка выкладки выкладку выкатка выкатки выкатку deploy deploys deployment",
		"test":        "тест теста тесты тестов тестами тестирование тестирования автотест автотесты автотестов test tests testing",
		"docs":        "документация документацию документации документацией дока доки доках documentation docs",
		"slow":        "долго долгий долгая долгие долгих долгого долгой дольше медленно медленный медленные медленная затянутые затягиваются slow slowly",
		"fast":        "быстро быстрее быстрый быстрая быстрые ускорить ускорим ускорять fast faster",
		"wait":        "ждем ждать ожидание ожидания ждали waits waiting",
		"fail":        "падает падают упал упали падение падения сломался сломались fail fails failing",
	} {
		for _, word := range strings.Fields(words) {
			terms[word] = topic
		}
	}
	return terms
}()

type cardTerms struct {
	id       string
	terms    []string
	negative bool
}

func groupTerms(card Card) cardTerms {
	text := strings.ReplaceAll(strings.ToLower(card.Text), "ё", "е")
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	distinct := make(map[string]bool)
	result := cardTerms{id: card.ID}
	for _, word := range words {
		switch word {
		case "не", "нет", "без", "not", "no", "without":
			result.negative = true
			continue
		}
		if alias, ok := groupingTerms[word]; ok {
			word = alias
		}
		if utf8.RuneCountInString(word) < 2 || distinct[word] {
			continue
		}
		distinct[word] = true
		result.terms = append(result.terms, word)
		// Ограничиваем стоимость попарных сравнений даже для длинных карточек.
		if len(result.terms) == 64 {
			break
		}
	}
	slices.Sort(result.terms)
	return result
}

func similarCards(a, b cardTerms) bool {
	if a.negative != b.negative || len(a.terms) == 0 || len(b.terms) == 0 {
		return false
	}
	overlap := 0
	for i, j := 0, 0; i < len(a.terms) && j < len(b.terms); {
		switch strings.Compare(a.terms[i], b.terms[j]) {
		case -1:
			i++
		case 1:
			j++
		default:
			overlap++
			i++
			j++
		}
	}
	if overlap < 2 {
		return overlap == 1 && len(a.terms) == 1 && len(b.terms) == 1
	}
	// Косинусная близость множеств: совпадение одной общей темы само по
	// себе недостаточно для двух развёрнутых карточек.
	return float64(overlap) >= .65*math.Sqrt(float64(len(a.terms)*len(b.terms)))
}

func suggestColumn(col Column) [][]string {
	var clusters [][]cardTerms
	for _, source := range col.Cards {
		if source.GroupID != "" {
			continue
		}
		card := groupTerms(source)
		if len(card.terms) == 0 {
			continue
		}
		// Сходство со всеми, а не цепочка через одну двусмысленную карточку.
		index := slices.IndexFunc(clusters, func(cluster []cardTerms) bool {
			for _, member := range cluster {
				if !similarCards(card, member) {
					return false
				}
			}
			return true
		})
		if index < 0 {
			clusters = append(clusters, []cardTerms{card})
		} else {
			clusters[index] = append(clusters[index], card)
		}
	}
	var result [][]string
	for _, cluster := range clusters {
		if len(cluster) > 1 {
			ids := make([]string, 0, len(cluster))
			for _, card := range cluster {
				ids = append(ids, card.id)
			}
			result = append(result, ids)
		}
	}
	return result
}
