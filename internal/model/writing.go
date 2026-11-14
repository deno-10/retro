package model

import "fmt"

const MaxWritingParticipants = 1000

// WritingState хранит состав этапа, а не открытые соединения: обрыв связи
// не равен нажатию «Я закончил». Один ID считается один раз во всех вкладках.
type WritingState struct {
	Round        uint64                        `json:"round"`
	Participants map[string]WritingParticipant `json:"participants"`
}

type WritingParticipant struct {
	Ready    bool   `json:"ready"`
	Revision uint64 `json:"revision"`
}

// WritingView не раскрывает идентификаторы других участников.
type WritingView struct {
	Round        uint64 `json:"round"`
	Participants int    `json:"participants"`
	Ready        int    `json:"ready"`
	Joined       bool   `json:"joined"`
	Mine         bool   `json:"mine"`
	Revision     uint64 `json:"revision"`
}

func (b *Board) writingState() *WritingState {
	if b.Writing != nil {
		return b.Writing
	}
	// Старые доски читаются без миграции. Учитываем ведущего и авторов уже
	// сохранённых карточек; остальные присоединятся при открытии страницы.
	s := &WritingState{Round: 1, Participants: map[string]WritingParticipant{}}
	s.Participants[b.OwnerID] = WritingParticipant{}
	for _, column := range b.Columns {
		for _, card := range column.Cards {
			s.Participants[card.AuthorID] = WritingParticipant{}
		}
	}
	return s
}

func (b *Board) writingView(viewer string) *WritingView {
	if b.Phase != PhaseWrite {
		return nil
	}
	s := b.writingState()
	p, joined := s.Participants[viewer]
	v := &WritingView{Round: s.Round, Participants: len(s.Participants), Joined: joined, Mine: p.Ready, Revision: p.Revision}
	for _, participant := range s.Participants {
		if participant.Ready {
			v.Ready++
		}
	}
	return v
}

// JoinWriting регистрирует участника текущего этапа. Повторное открытие
// страницы не снимает готовность и не меняет численность команды.
func (b *Board) JoinWriting(actorID string, round uint64) (bool, error) {
	if b.Phase != PhaseWrite {
		return false, ErrPhase
	}
	s := b.writingState()
	if s.Round != round {
		return false, ErrConflict
	}
	if _, exists := s.Participants[actorID]; exists {
		changed := b.Writing == nil
		b.Writing = s
		return changed, nil
	}
	if len(s.Participants) >= MaxWritingParticipants {
		return false, fmt.Errorf("%w: не более %d участников этапа", ErrCapacity, MaxWritingParticipants)
	}
	s.Participants[actorID] = WritingParticipant{}
	b.Writing = s
	return true, nil
}

// SetWritingReady принимает желаемое значение и версию отметки участника.
// Последняя отметка и переход сохраняются одной транзакцией хранилища.
func (b *Board) SetWritingReady(actorID string, ready bool, round, revision uint64) (bool, error) {
	s := b.writingState()
	if s.Round != round {
		return false, ErrConflict
	}
	p, joined := s.Participants[actorID]
	if !joined {
		return false, fmt.Errorf("%w: сначала присоединитесь к этапу записи", ErrForbidden)
	}
	// Потерянный ответ на последнее нажатие можно повторить и после показа.
	// Версия защищает от старого true после отмены или новой правки карточки.
	if p.Ready == ready && (p.Revision == revision || (p.Revision > 0 && p.Revision-1 == revision)) {
		return false, nil
	}
	if b.Phase != PhaseWrite {
		return false, ErrPhase
	}
	if p.Revision != revision {
		return false, ErrConflict
	}
	p.Ready, p.Revision = ready, p.Revision+1
	s.Participants[actorID] = p
	b.Writing = s
	if ready {
		for _, other := range s.Participants {
			if !other.Ready {
				return true, nil
			}
		}
		b.Phase = PhaseReveal
		b.TimerEndsAt = nil
	}
	return true, nil
}

// Любая сохранённая правка отменяет прежнюю готовность. Версия меняется
// также у неготового участника, чтобы запоздавшее нажатие её не обогнало.
func (b *Board) noteWritingActivity(actorID string) error {
	if b.Phase != PhaseWrite {
		return nil
	}
	if _, err := b.JoinWriting(actorID, b.writingState().Round); err != nil {
		return err
	}
	p := b.Writing.Participants[actorID]
	p.Ready = false
	p.Revision++
	b.Writing.Participants[actorID] = p
	return nil
}

func (b *Board) restartWriting() {
	old := b.writingState()
	next := &WritingState{Round: old.Round + 1, Participants: map[string]WritingParticipant{}}
	for id := range old.Participants {
		next.Participants[id] = WritingParticipant{}
	}
	next.Participants[b.OwnerID] = WritingParticipant{}
	b.Writing = next
}
