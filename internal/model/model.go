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
	MaxTimerMinutes  = 120
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
	ID        string `json:"id"`
	Title     string `json:"title"`
	Template  string `json:"template"`
	OwnerID   string `json:"ownerId"` // ведущий: создатель доски
	Phase     Phase  `json:"phase"`
	VoteLimit int    `json:"voteLimit"` // голосов на участника
	// TimerEndsAt — конец таймера текущей фазы; nil — таймер не запущен.
	// Таймер только информирует: фазу по его истечении переключает ведущий.
	TimerEndsAt *time.Time `json:"timerEndsAt,omitempty"`
	Columns     []Column   `json:"columns"`
	Actions     []Action   `json:"actions"` // решения ретро
	// PrevID и NextID связывают ретро одной команды в цепочку: следующее
	// ретро создаётся из предыдущего и забирает его незакрытые решения.
	PrevID    string    `json:"prevId,omitempty"`
	NextID    string    `json:"nextId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Action — решение ретро (action item): что сделать и кто отвечает.
// Появляются после фазы записи, когда карточки уже обсуждаются.
type Action struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Assignee string `json:"assignee"` // ответственный; пустая строка — не назначен
	AuthorID string `json:"authorId"` // кто записал решение
	Done     bool   `json:"done"`
	// CarriedFrom — id ретро, из которого решение перенесено незакрытым.
	CarriedFrom string    `json:"carriedFrom,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
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
	Author    string    `json:"author"`            // подпись; пустая — анонимно
	AuthorID  string    `json:"authorId"`          // идентификатор участника
	Votes     []string  `json:"votes"`             // идентификаторы проголосовавших
	GroupID   string    `json:"groupId,omitempty"` // карточки с одним GroupID склеены в группу
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

// NextBoard создаёт следующее ретро той же команды: тот же шаблон и ведущий,
// ссылка на предыдущее, незакрытые решения переносятся с пометкой откуда.
// Только ведущий, и только если следующего ещё нет. Связь prev.NextID
// выставляет вызывающий код после успешного сохранения новой доски.
func (prev *Board) NextBoard(title, actorID string) (*Board, error) {
	if actorID != prev.OwnerID {
		return nil, ErrForbidden
	}
	if prev.NextID != "" {
		return nil, fmt.Errorf("%w: у этого ретро уже есть следующее", ErrInvalid)
	}
	if strings.TrimSpace(title) == "" {
		title = prev.Title
	}
	b, err := NewBoard(title, prev.Template, prev.OwnerID)
	if err != nil {
		return nil, err
	}
	b.PrevID = prev.ID
	b.VoteLimit = prev.VoteLimit
	for _, a := range prev.Actions {
		if a.Done {
			continue
		}
		b.Actions = append(b.Actions, Action{
			ID:          NewID(),
			Text:        a.Text,
			Assignee:    a.Assignee,
			AuthorID:    a.AuthorID,
			CarriedFrom: prev.ID,
			CreatedAt:   b.CreatedAt,
		})
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
	group := c.GroupID
	for i := range col.Cards {
		if col.Cards[i].ID == cardID {
			col.Cards = append(col.Cards[:i], col.Cards[i+1:]...)
			break
		}
	}
	dissolveIfSingle(col, group)
	return nil
}

// dissolveIfSingle снимает группу, в которой осталась одна карточка:
// группа из одной карточки — просто карточка.
func dissolveIfSingle(col *Column, groupID string) {
	if groupID == "" {
		return
	}
	var only *Card
	n := 0
	for i := range col.Cards {
		if col.Cards[i].GroupID == groupID {
			n++
			only = &col.Cards[i]
		}
	}
	if n == 1 {
		only.GroupID = ""
	}
}

// canGroup: группирует только ведущий и только когда карточки видны всем.
func (b *Board) canGroup(actorID string) error {
	if actorID != b.OwnerID {
		return ErrForbidden
	}
	if b.Phase == PhaseWrite {
		return ErrPhase
	}
	return nil
}

// GroupCards присоединяет карточку cardID к карточке targetID: если цель уже
// в группе — к её группе, иначе из двух карточек создаётся новая группа.
// Карточки должны быть в одной колонке.
func (b *Board) GroupCards(cardID, targetID, actorID string) error {
	if err := b.canGroup(actorID); err != nil {
		return err
	}
	if cardID == targetID {
		return fmt.Errorf("%w: карточку нельзя сгруппировать с самой собой", ErrInvalid)
	}
	col, c := b.findCard(cardID)
	tcol, t := b.findCard(targetID)
	if c == nil || t == nil {
		return fmt.Errorf("%w: карточка не найдена", ErrNotFound)
	}
	if col != tcol {
		return fmt.Errorf("%w: группировать можно только в пределах колонки", ErrInvalid)
	}
	if c.GroupID != "" && c.GroupID == t.GroupID {
		return nil // уже вместе
	}
	old := c.GroupID
	if t.GroupID == "" {
		t.GroupID = NewID()
	}
	c.GroupID = t.GroupID
	dissolveIfSingle(col, old)
	return nil
}

// UngroupCard вынимает карточку из группы.
func (b *Board) UngroupCard(cardID, actorID string) error {
	if err := b.canGroup(actorID); err != nil {
		return err
	}
	col, c := b.findCard(cardID)
	if c == nil {
		return fmt.Errorf("%w: карточка не найдена", ErrNotFound)
	}
	old := c.GroupID
	c.GroupID = ""
	dissolveIfSingle(col, old)
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
	col, c := b.findCard(cardID)
	if c == nil {
		return false, fmt.Errorf("%w: карточка не найдена", ErrNotFound)
	}
	// За группу голосуют как за одну карточку: если голос участника уже
	// стоит на любой карточке группы, повторный клик его снимает.
	members := []*Card{c}
	if c.GroupID != "" {
		members = members[:0]
		for i := range col.Cards {
			if col.Cards[i].GroupID == c.GroupID {
				members = append(members, &col.Cards[i])
			}
		}
	}
	for _, m := range members {
		for i, v := range m.Votes {
			if v == participantID {
				m.Votes = append(m.Votes[:i], m.Votes[i+1:]...)
				return false, nil
			}
		}
	}
	if b.VotesUsed(participantID) >= b.VoteLimit {
		return false, ErrVotes
	}
	c.Votes = append(c.Votes, participantID)
	return true, nil
}

// groupVotes — сумма голосов карточек группы.
func groupVotes(col Column, groupID string) int {
	n := 0
	for _, c := range col.Cards {
		if c.GroupID == groupID {
			n += len(c.Votes)
		}
	}
	return n
}

// groupVoted — голосовал ли участник за какую-либо карточку группы.
func groupVoted(col Column, groupID, participantID string) bool {
	for _, c := range col.Cards {
		if c.GroupID != groupID {
			continue
		}
		for _, v := range c.Votes {
			if v == participantID {
				return true
			}
		}
	}
	return false
}

// arrange возвращает карточки колонки так, чтобы карточки одной группы шли
// подряд на месте первой из них. При byVotes порядок — по убыванию голосов
// группы (для одиночной карточки — её голосов), при равенстве исходный.
func arrange(col Column, byVotes bool) []Card {
	anchor := map[string]int{} // группа → индекс её первой карточки
	for i, c := range col.Cards {
		if c.GroupID != "" {
			if _, ok := anchor[c.GroupID]; !ok {
				anchor[c.GroupID] = i
			}
		}
	}
	pos := func(i int) int {
		if g := col.Cards[i].GroupID; g != "" {
			return anchor[g]
		}
		return i
	}
	weight := func(i int) int {
		if g := col.Cards[i].GroupID; g != "" {
			return groupVotes(col, g)
		}
		return len(col.Cards[i].Votes)
	}
	idx := make([]int, len(col.Cards))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		if byVotes {
			if wa, wb := weight(idx[a]), weight(idx[b]); wa != wb {
				return wa > wb
			}
		}
		return pos(idx[a]) < pos(idx[b])
	})
	out := make([]Card, len(idx))
	for i, j := range idx {
		out[i] = col.Cards[j]
	}
	return out
}

// SetPhase переводит доску в фазу. Только ведущий; в любом направлении.
func (b *Board) SetPhase(p Phase, actorID string) error {
	if actorID != b.OwnerID {
		return ErrForbidden
	}
	if !p.Valid() {
		return fmt.Errorf("%w: неизвестная фаза %q", ErrInvalid, string(p))
	}
	if p != b.Phase {
		b.TimerEndsAt = nil // таймер относится к фазе и вместе с ней заканчивается
	}
	b.Phase = p
	return nil
}

func (b *Board) findAction(actionID string) *Action {
	for i := range b.Actions {
		if b.Actions[i].ID == actionID {
			return &b.Actions[i]
		}
	}
	return nil
}

func cleanAssignee(assignee string) (string, error) {
	assignee = strings.TrimSpace(assignee)
	if utf8.RuneCountInString(assignee) > MaxNameLen {
		return "", fmt.Errorf("%w: имя ответственного длиннее %d символов", ErrInvalid, MaxNameLen)
	}
	return assignee, nil
}

// canEditAction: решение правят ведущий и тот, кто его записал.
// Фаза не важна: решения уточняют и после ретро.
func (b *Board) canEditAction(a *Action, actorID string) error {
	if actorID == b.OwnerID || a.AuthorID == actorID {
		return nil
	}
	return ErrForbidden
}

// AddAction записывает решение. Недоступно в фазе записи: решения
// появляются, когда карточки уже видны всем.
func (b *Board) AddAction(text, assignee, authorID string) (*Action, error) {
	if b.Phase == PhaseWrite {
		return nil, ErrPhase
	}
	text, err := cleanText(text)
	if err != nil {
		return nil, err
	}
	assignee, err = cleanAssignee(assignee)
	if err != nil {
		return nil, err
	}
	b.Actions = append(b.Actions, Action{
		ID:        NewID(),
		Text:      text,
		Assignee:  assignee,
		AuthorID:  authorID,
		CreatedAt: time.Now(),
	})
	return &b.Actions[len(b.Actions)-1], nil
}

// EditAction меняет текст и ответственного решения.
func (b *Board) EditAction(actionID, text, assignee, actorID string) error {
	a := b.findAction(actionID)
	if a == nil {
		return fmt.Errorf("%w: решение не найдено", ErrNotFound)
	}
	if err := b.canEditAction(a, actorID); err != nil {
		return err
	}
	text, err := cleanText(text)
	if err != nil {
		return err
	}
	assignee, err = cleanAssignee(assignee)
	if err != nil {
		return err
	}
	a.Text, a.Assignee = text, assignee
	return nil
}

// ToggleActionDone переключает отметку «сделано». Возвращает новое состояние.
func (b *Board) ToggleActionDone(actionID, actorID string) (bool, error) {
	a := b.findAction(actionID)
	if a == nil {
		return false, fmt.Errorf("%w: решение не найдено", ErrNotFound)
	}
	if err := b.canEditAction(a, actorID); err != nil {
		return false, err
	}
	a.Done = !a.Done
	return a.Done, nil
}

// DeleteAction удаляет решение.
func (b *Board) DeleteAction(actionID, actorID string) error {
	a := b.findAction(actionID)
	if a == nil {
		return fmt.Errorf("%w: решение не найдено", ErrNotFound)
	}
	if err := b.canEditAction(a, actorID); err != nil {
		return err
	}
	for i := range b.Actions {
		if b.Actions[i].ID == actionID {
			b.Actions = append(b.Actions[:i], b.Actions[i+1:]...)
			return nil
		}
	}
	return nil
}

// SetTimer запускает таймер фазы на minutes минут от now. Только ведущий.
// minutes == 0 останавливает таймер.
func (b *Board) SetTimer(minutes int, now time.Time, actorID string) error {
	if actorID != b.OwnerID {
		return ErrForbidden
	}
	if minutes < 0 || minutes > MaxTimerMinutes {
		return fmt.Errorf("%w: таймер от 1 до %d минут, 0 — остановить", ErrInvalid, MaxTimerMinutes)
	}
	if minutes == 0 {
		b.TimerEndsAt = nil
		return nil
	}
	ends := now.Add(time.Duration(minutes) * time.Minute)
	b.TimerEndsAt = &ends
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
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Phase       Phase        `json:"phase"`
	VoteLimit   int          `json:"voteLimit"`
	VotesLeft   int          `json:"votesLeft"`
	IsOwner     bool         `json:"isOwner"`
	TimerEndsAt *time.Time   `json:"timerEndsAt,omitempty"`
	Columns     []ColumnView `json:"columns"`
	Actions     []ActionView `json:"actions"`
	PrevID      string       `json:"prevId,omitempty"`
	NextID      string       `json:"nextId,omitempty"`
	UpdatedAt   time.Time    `json:"updatedAt"`
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
	// Группа: карточки с одним groupId идут в списке подряд. GroupVotes и
	// GroupVoted — по группе целиком, за неё голосуют как за одну карточку.
	GroupID    string `json:"groupId,omitempty"`
	GroupVotes int    `json:"groupVotes"`
	GroupVoted bool   `json:"groupVoted"`
}

// ActionView — решение в представлении участника.
type ActionView struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Assignee string `json:"assignee"`
	Done     bool   `json:"done"`
	Mine     bool   `json:"mine"`    // записано этим участником — можно править
	Carried  bool   `json:"carried"` // перенесено из предыдущего ретро
}

// View строит представление доски для участника viewer.
func (b *Board) View(viewer string) BoardView {
	showVotes := b.Phase == PhaseVote || b.Phase == PhaseDiscuss
	v := BoardView{
		ID:          b.ID,
		Title:       b.Title,
		Phase:       b.Phase,
		VoteLimit:   b.VoteLimit,
		VotesLeft:   b.VoteLimit - b.VotesUsed(viewer),
		IsOwner:     viewer == b.OwnerID,
		TimerEndsAt: b.TimerEndsAt,
		Columns:     make([]ColumnView, 0, len(b.Columns)),
		PrevID:      b.PrevID,
		NextID:      b.NextID,
		UpdatedAt:   b.UpdatedAt,
	}
	if v.VotesLeft < 0 {
		v.VotesLeft = 0
	}
	for _, col := range b.Columns {
		cv := ColumnView{ID: col.ID, Title: col.Title, Cards: make([]CardView, 0, len(col.Cards))}
		for _, c := range arrange(col, b.Phase == PhaseDiscuss) {
			card := CardView{ID: c.ID, Mine: c.AuthorID == viewer, GroupID: c.GroupID}
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
				if c.GroupID != "" {
					card.GroupVotes = groupVotes(col, c.GroupID)
					card.GroupVoted = groupVoted(col, c.GroupID, viewer)
				}
			}
			cv.Cards = append(cv.Cards, card)
		}
		v.Columns = append(v.Columns, cv)
	}
	v.Actions = make([]ActionView, 0, len(b.Actions))
	for _, a := range b.Actions {
		v.Actions = append(v.Actions, ActionView{
			ID: a.ID, Text: a.Text, Assignee: a.Assignee, Done: a.Done,
			Mine: a.AuthorID == viewer, Carried: a.CarriedFrom != "",
		})
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
		cards := arrange(col, true)
		if len(cards) == 0 {
			sb.WriteString("_пусто_\n\n")
			continue
		}
		// Группа — вложенный список: первая карточка с суммой голосов группы,
		// остальные под ней без голосов.
		seen := map[string]bool{}
		for _, c := range cards {
			indent := ""
			votes := len(c.Votes)
			if c.GroupID != "" {
				if seen[c.GroupID] {
					indent = "  "
					votes = 0
				} else {
					seen[c.GroupID] = true
					votes = groupVotes(col, c.GroupID)
				}
			}
			text := strings.ReplaceAll(strings.TrimSpace(c.Text), "\n", "\n"+indent+"  ")
			fmt.Fprintf(&sb, "%s- %s", indent, text)
			if c.Author != "" {
				fmt.Fprintf(&sb, " — %s", c.Author)
			}
			if votes > 0 {
				fmt.Fprintf(&sb, " (%d %s)", votes, plural(votes, "голос", "голоса", "голосов"))
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}
	if len(b.Actions) > 0 {
		sb.WriteString("## Решения\n\n")
		for _, a := range b.Actions {
			mark := " "
			if a.Done {
				mark = "x"
			}
			text := strings.ReplaceAll(strings.TrimSpace(a.Text), "\n", "\n  ")
			fmt.Fprintf(&sb, "- [%s] %s", mark, text)
			if a.Assignee != "" {
				fmt.Fprintf(&sb, " — %s", a.Assignee)
			}
			if a.CarriedFrom != "" {
				sb.WriteString(" (перенесено)")
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
