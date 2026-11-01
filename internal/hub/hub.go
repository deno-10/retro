// Package hub рассылает подписчикам доски сигнал «состояние изменилось».
// Сам сигнал данных не несёт: получатель сам перечитывает доску из
// хранилища и строит представление для своего участника.
package hub

import "sync"

// Hub — подписки, сгруппированные по доске.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
	done chan struct{}
	once sync.Once
}

// New создаёт пустой Hub.
func New() *Hub {
	return &Hub{
		subs: map[string]map[chan struct{}]struct{}{},
		done: make(chan struct{}),
	}
}

// Subscribe возвращает канал сигналов по доске и функцию отписки.
func (h *Hub) Subscribe(boardID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[boardID] == nil {
		h.subs[boardID] = map[chan struct{}]struct{}{}
	}
	h.subs[boardID][ch] = struct{}{}
	h.mu.Unlock()

	unsubscribe := func() {
		h.mu.Lock()
		if set := h.subs[boardID]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.subs, boardID)
			}
		}
		h.mu.Unlock()
	}
	return ch, unsubscribe
}

// Notify будит всех подписчиков доски. Сигналы схлопываются: если подписчик
// ещё не обработал предыдущий, новый не копится — он всё равно перечитает
// актуальное состояние.
func (h *Hub) Notify(boardID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[boardID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Count — сколько подписчиков (открытых вкладок) у доски сейчас.
func (h *Hub) Count(boardID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs[boardID])
}

// Done закрывается при остановке сервера: все SSE-соединения должны завершиться.
func (h *Hub) Done() <-chan struct{} { return h.done }

// Close сигналит всем соединениям завершиться. Повторные вызовы безопасны.
func (h *Hub) Close() { h.once.Do(func() { close(h.done) }) }
