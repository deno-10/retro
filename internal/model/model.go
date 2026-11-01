// Package model описывает предметную область ретро: доску, колонки, карточки,
// фазы и голосование. Здесь нет ни HTTP, ни хранилища — только правила.
package model

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Phase — фаза ретро. Порядок фиксирован: write → reveal → vote → discuss.
type Phase string

const (
	PhaseWrite   Phase = "write"   // участники пишут карточки, чужие скрыты
	PhaseReveal  Phase = "reveal"  // все карточки видны, можно дописывать
	PhaseVote    Phase = "vote"    // голосование точками
	PhaseDiscuss Phase = "discuss" // карточки отсортированы по голосам
)

// Phases — все фазы в порядке прохождения.
var Phases = []Phase{PhaseWrite, PhaseReveal, PhaseVote, PhaseDiscuss}

// Valid сообщает, известна ли фаза.
func (p Phase) Valid() bool {
	for _, q := range Phases {
		if p == q {
			return true
		}
	}
	return false
}

// Ограничения на ввод.
const (
	MaxTitleLen      = 120
	MaxTextLen       = 2000
	MaxNameLen       = 60
	DefaultVoteLimit = 5
	MaxVoteLimit     = 20
)

// Ошибки предметной области. HTTP-слой превращает их в коды ответа.
var (
	ErrNotFound  = errors.New("не найдено")
	ErrForbidden = errors.New("нет прав на это действие")
	ErrPhase     = errors.New("в текущей фазе это действие недоступно")
	ErrVotes     = errors.New("голоса закончились")
	ErrInvalid   = errors.New("некорректные данные")
)

// Board — доска одного ретро.
type Board struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Template  string    `json:"template"`
	OwnerID   string    `json:"ownerId"` // ведущий: создатель доски
	Phase     Phase     `json:"phase"`
	VoteLimit int       `json:"voteLimit"` // голосов на участника
	Columns   []Column  `json:"columns"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Column — колонка доски.
type Column struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Cards []Card `json:"cards"`
}

// Card — карточка.
type Card struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	Author    string    `json:"author"`   // подпись; пустая — анонимно
	AuthorID  string    `json:"authorId"` // идентификатор участника
	Votes     []string  `json:"votes"`    // идентификаторы проголосовавших
	CreatedAt time.Time `json:"createdAt"`
}

// Template — набор колонок для новой доски.
type Template struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
}

// Templates — доступные шаблоны.
var Templates = []Template{
	{ID: "classic", Name: "Классика", Columns: []string{"Что было хорошо", "Что улучшить", "Что сделать"}},
	{ID: "ssc", Name: "Start / Stop / Continue", Columns: []string{"Начать", "Перестать", "Продолжать"}},
	{ID: "4l", Name: "4L", Columns: []string{"Понравилось", "Узнали", "Не хватало", "Хотелось бы"}},
	{ID: "msg", Name: "Mad / Sad / Glad", Columns: []string{"Злит", "Огорчает", "Радует"}},
}

// TemplateByID находит шаблон по идентификатору.
func TemplateByID(id string) (Template, bool) {
	for _, t := range Templates {
		if t.ID == id {
			return t, true
		}
	}
	return Template{}, false
}

// NewID возвращает случайный идентификатор: 16 шестнадцатеричных символов.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand недоступен: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// NewBoard создаёт доску по шаблону. Создатель становится ведущим.
func NewBoard(title, templateID, ownerID string) (*Board, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "Ретро"
	}
	if utf8.RuneCountInString(title) > MaxTitleLen {
		return nil, fmt.Errorf("%w: название длиннее %d символов", ErrInvalid, MaxTitleLen)
	}
	if templateID == "" {
		templateID = "classic"
	}
	t, ok := TemplateByID(templateID)
	if !ok {
		return nil, fmt.Errorf("%w: неизвестный шаблон %q", ErrInvalid, templateID)
	}
	if ownerID == "" {
		return nil, fmt.Errorf("%w: нет идентификатора участника", ErrInvalid)
	}
	now := time.Now()
	b := &Board{
		ID:        NewID(),
		Title:     title,
		Template:  t.ID,
		OwnerID:   ownerID,
		Phase:     PhaseWrite,
		VoteLimit: DefaultVoteLimit,
		CreatedAt: now,
		UpdatedAt: now,
	}
	for _, name := range t.Columns {
		b.Columns = append(b.Columns, Column{ID: NewID(), Title: name, Cards: []Card{}})
	}
	return b, nil
}

func (b *Board) findCard(cardID string) (*Column, *Card) {
	for i := range b.Columns {
		for j := range b.Columns[i].Cards {
			if b.Columns[i].Cards[j].ID == cardID {
				return &b.Columns[i], &b.Columns[i].Cards[j]
			}
		}
	}
	return nil, nil
}

func cleanText(text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("%w: текст карточки пустой", ErrInvalid)
	}
	if utf8.RuneCountInString(text) > MaxTextLen {
		return "", fmt.Errorf("%w: текст длиннее %d символов", ErrInvalid, MaxTextLen)
	}
	return text, nil
}

// AddCard добавляет карточку в колонку. Разрешено в фазах write и reveal.
func (b *Board) AddCard(columnID, text, author, authorID string) (*Card, error) {
	if b.Phase != PhaseWrite && b.Phase != PhaseReveal {
		return nil, ErrPhase
	}
	text, err := cleanText(text)
	if err != nil {
		return nil, err
	}
	author = strings.TrimSpace(author)
	if utf8.RuneCountInString(author) > MaxNameLen {
		return nil, fmt.Errorf("%w: имя длиннее %d символов", ErrInvalid, MaxNameLen)
	}
	for i := range b.Columns {
		if b.Columns[i].ID == columnID {
			c := Card{
				ID:        NewID(),
				Text:      text,
				Author:    author,
				AuthorID:  authorID,
				Votes:     []string{},
				CreatedAt: time.Now(),
			}
			b.Columns[i].Cards = append(b.Columns[i].Cards, c)
			return &b.Columns[i].Cards[len(b.Columns[i].Cards)-1], nil
		}
	}
	return nil, fmt.Errorf("%w: колонка не найдена", ErrNotFound)
}

// canEdit: ведущий может править любую карточку в любой фазе,
// автор — только свою и только пока идёт запись или показ.
func (b *Board) canEdit(c *Card, actorID string) error {
	if actorID == b.OwnerID {
		return nil
	}
	if c.AuthorID != actorID {
		return ErrForbidden
	}
	if b.Phase != PhaseWrite && b.Phase != PhaseReveal {
		return ErrPhase
	}
	return nil
}

// EditCard меняет текст карточки.
func (b *Board) EditCard(cardID, text, actorID string) error {
	_, c := b.findCard(cardID)
	if c == nil {
		return fmt.Errorf("%w: карточка не найдена", ErrNotFound)
	}
	if err := b.canEdit(c, actorID); err != nil {
		return err
	}
	text, err := cleanText(text)
	if err != nil {
		return err
	}
	c.Text = text
	return nil
}

// DeleteCard удаляет карточку.
func (b *Board) DeleteCard(cardID, actorID string) error {
	col, c := b.findCard(cardID)
	if c == nil {
		return fmt.Errorf("%w: карточка не найдена", ErrNotFound)
	}
	if err := b.canEdit(c, actorID); err != nil {
		return err
	}
	for i := range col.Cards {
		if col.Cards[i].ID == cardID {
			col.Cards = append(col.Cards[:i], col.Cards[i+1:]...)
			return nil
		}
	}
	return nil
}

// VotesUsed — сколько голосов участник уже потратил.
func (b *Board) VotesUsed(participantID string) int {
	n := 0
	for i := range b.Columns {
		for j := range b.Columns[i].Cards {
			for _, v := range b.Columns[i].Cards[j].Votes {
				if v == participantID {
					n++
				}
			}
		}
	}
	return n
}

// ToggleVote ставит голос на карточку или снимает его.
// Возвращает true, если после вызова голос стоит.
func (b *Board) ToggleVote(cardID, participantID string) (bool, error) {
	if b.Phase != PhaseVote {
		return false, ErrPhase
	}
	_, c := b.findCard(cardID)
	if c == nil {
		return false, fmt.Errorf("%w: карточка не найдена", ErrNotFound)
	}
	for i, v := range c.Votes {
		if v == participantID {
			c.Votes = append(c.Votes[:i], c.Votes[i+1:]...)
			return false, nil
		}
	}
	if b.VotesUsed(participantID) >= b.VoteLimit {
		return false, ErrVotes
	}
	c.Votes = append(c.Votes, participantID)
	return true, nil
}

// SetPhase переводит доску в фазу. Только ведущий; в любом направлении.
func (b *Board) SetPhase(p Phase, actorID string) error {
	if actorID != b.OwnerID {
		return ErrForbidden
	}
	if !p.Valid() {
		return fmt.Errorf("%w: неизвестная фаза %q", ErrInvalid, string(p))
	}
	b.Phase = p
	return nil
}

// SetVoteLimit задаёт число голосов на участника. Только ведущий.
func (b *Board) SetVoteLimit(n int, actorID string) error {
	if actorID != b.OwnerID {
		return ErrForbidden
	}
	if n < 1 || n > MaxVoteLimit {
		return fmt.Errorf("%w: лимит голосов от 1 до %d", ErrInvalid, MaxVoteLimit)
	}
	b.VoteLimit = n
	return nil
}

// BoardView — доска глазами конкретного участника: чужие карточки в фазе
// записи скрыты, голоса видны только с фазы голосования.
type BoardView struct {
	ID        string       `json:"id"`
	Title     string       `json:"title"`
	Phase     Phase        `json:"phase"`
	VoteLimit int          `json:"voteLimit"`
	VotesLeft int          `json:"votesLeft"`
	IsOwner   bool         `json:"isOwner"`
	Columns   []ColumnView `json:"columns"`
	UpdatedAt time.Time    `json:"updatedAt"`
}

// ColumnView — колонка в представлении участника.
type ColumnView struct {
	ID    string     `json:"id"`
	Title string     `json:"title"`
	Cards []CardView `json:"cards"`
}

// CardView — карточка в представлении участника.
type CardView struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Author string `json:"author"`
	Mine   bool   `json:"mine"`
	Hidden bool   `json:"hidden"`
	Votes  int    `json:"votes"`
	Voted  bool   `json:"voted"`
}

// View строит представление доски для участника viewer.
func (b *Board) View(viewer string) BoardView {
	showVotes := b.Phase == PhaseVote || b.Phase == PhaseDiscuss
	v := BoardView{
		ID:        b.ID,
		Title:     b.Title,
		Phase:     b.Phase,
		VoteLimit: b.VoteLimit,
		VotesLeft: b.VoteLimit - b.VotesUsed(viewer),
		IsOwner:   viewer == b.OwnerID,
		Columns:   make([]ColumnView, 0, len(b.Columns)),
		UpdatedAt: b.UpdatedAt,
	}
	if v.VotesLeft < 0 {
		v.VotesLeft = 0
	}
	for _, col := range b.Columns {
		cv := ColumnView{ID: col.ID, Title: col.Title, Cards: make([]CardView, 0, len(col.Cards))}
		for _, c := range col.Cards {
			card := CardView{ID: c.ID, Mine: c.AuthorID == viewer}
			if b.Phase == PhaseWrite && !card.Mine {
				card.Hidden = true
			} else {
				card.Text = c.Text
				card.Author = c.Author
			}
			if showVotes {
				card.Votes = len(c.Votes)
				for _, p := range c.Votes {
					if p == viewer {
						card.Voted = true
						break
					}
				}
			}
			cv.Cards = append(cv.Cards, card)
		}
		if b.Phase == PhaseDiscuss {
			sort.SliceStable(cv.Cards, func(i, j int) bool { return cv.Cards[i].Votes > cv.Cards[j].Votes })
		}
		v.Columns = append(v.Columns, cv)
	}
	return v
}

// Markdown — экспорт доски: колонки, карточки по убыванию голосов.
func (b *Board) Markdown() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s\n\n", b.Title)
	fmt.Fprintf(&sb, "%s\n\n", b.CreatedAt.Format("02.01.2006"))
	for _, col := range b.Columns {
		fmt.Fprintf(&sb, "## %s\n\n", col.Title)
		cards := append([]Card(nil), col.Cards...)
		sort.SliceStable(cards, func(i, j int) bool { return len(cards[i].Votes) > len(cards[j].Votes) })
		if len(cards) == 0 {
			sb.WriteString("_пусто_\n\n")
			continue
		}
		for _, c := range cards {
			text := strings.ReplaceAll(strings.TrimSpace(c.Text), "\n", "\n  ")
			fmt.Fprintf(&sb, "- %s", text)
			if c.Author != "" {
				fmt.Fprintf(&sb, " — %s", c.Author)
			}
			if n := len(c.Votes); n > 0 {
				fmt.Fprintf(&sb, " (%d %s)", n, plural(n, "голос", "голоса", "голосов"))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func plural(n int, one, few, many string) string {
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return one
	case n10 >= 2 && n10 <= 4 && (n100 < 10 || n100 >= 20):
		return few
	default:
		return many
	}
}
