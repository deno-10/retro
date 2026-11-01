// Package server — HTTP-слой: REST API для мутаций, SSE-поток для обновлений,
// отдача статики. Бизнес-правил здесь нет, они в model.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"retro/internal/hub"
	"retro/internal/model"
	"retro/internal/store"
)

// Server держит зависимости обработчиков.
type Server struct {
	store store.Store
	hub   *hub.Hub
	log   *slog.Logger
	index []byte

	// timers — запланированные срабатывания таймеров фаз по id доски.
	// Планировщик живёт в памяти процесса; после рестарта таймеры
	// восстанавливаются лениво — при первом чтении доски (syncTimer).
	mu     sync.Mutex
	timers map[string]*time.Timer
}

// errTimerNotDue — таймер ещё не истёк (или уже снят): Update откатывается.
var errTimerNotDue = errors.New("таймер ещё не истёк")

// New собирает маршрутизатор. static — содержимое каталога web/.
func New(st store.Store, h *hub.Hub, static fs.FS, log *slog.Logger) http.Handler {
	index, err := fs.ReadFile(static, "index.html")
	if err != nil {
		panic("web/index.html не найден: " + err.Error())
	}
	s := &Server{store: st, hub: h, log: log, index: index, timers: map[string]*time.Timer{}}

	m := http.NewServeMux()
	m.HandleFunc("GET /api/templates", s.handleTemplates)
	m.HandleFunc("POST /api/boards", s.handleCreateBoard)
	m.HandleFunc("GET /api/boards/{id}", s.handleGetBoard)
	m.HandleFunc("GET /api/boards/{id}/events", s.handleEvents)
	m.HandleFunc("GET /api/boards/{id}/export.md", s.handleExport)
	m.HandleFunc("POST /api/boards/{id}/phase", s.handleSetPhase)
	m.HandleFunc("POST /api/boards/{id}/vote-limit", s.handleSetVoteLimit)
	m.HandleFunc("POST /api/boards/{id}/timer", s.handleSetTimer)
	m.HandleFunc("POST /api/boards/{id}/next", s.handleNextBoard)
	m.HandleFunc("GET /api/boards/{id}/history", s.handleHistory)
	m.HandleFunc("POST /api/boards/{id}/cards", s.handleAddCard)
	m.HandleFunc("PATCH /api/boards/{id}/cards/{cardId}", s.handleEditCard)
	m.HandleFunc("DELETE /api/boards/{id}/cards/{cardId}", s.handleDeleteCard)
	m.HandleFunc("POST /api/boards/{id}/cards/{cardId}/vote", s.handleVote)
	m.HandleFunc("POST /api/boards/{id}/cards/{cardId}/group", s.handleGroupCard)
	m.HandleFunc("DELETE /api/boards/{id}/cards/{cardId}/group", s.handleUngroupCard)
	m.HandleFunc("POST /api/boards/{id}/actions", s.handleAddAction)
	m.HandleFunc("PATCH /api/boards/{id}/actions/{actionId}", s.handleEditAction)
	m.HandleFunc("DELETE /api/boards/{id}/actions/{actionId}", s.handleDeleteAction)
	m.HandleFunc("POST /api/boards/{id}/actions/{actionId}/done", s.handleToggleActionDone)

	m.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	m.HandleFunc("GET /b/{id}", s.handleIndex)
	m.HandleFunc("GET /{$}", s.handleIndex)

	return s.withLogging(m)
}

// participant — идентификатор участника. Браузер генерирует его один раз
// и присылает в заголовке X-Participant (для SSE — в query, EventSource
// заголовки не умеет). Это не аутентификация; SSO появится позже.
func participant(r *http.Request) (string, error) {
	pid := r.Header.Get("X-Participant")
	if pid == "" {
		pid = r.URL.Query().Get("participant")
	}
	if len(pid) < 8 || len(pid) > 64 {
		return "", fmt.Errorf("%w: нет идентификатора участника", model.ErrInvalid)
	}
	for _, c := range pid {
		isLetter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if !isLetter && !isDigit && c != '-' {
			return "", fmt.Errorf("%w: некорректный идентификатор участника", model.ErrInvalid)
		}
	}
	return pid, nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("%w: тело запроса не разобрано (%v)", model.ErrInvalid, err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) writeErr(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, model.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, model.ErrForbidden):
		status = http.StatusForbidden
	case errors.Is(err, model.ErrPhase), errors.Is(err, model.ErrVotes):
		status = http.StatusConflict
	case errors.Is(err, model.ErrInvalid):
		status = http.StatusBadRequest
	}
	msg := err.Error()
	if status == http.StatusInternalServerError {
		s.log.Error("внутренняя ошибка", "err", err)
		msg = "внутренняя ошибка сервера"
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

// mutate применяет изменение к доске, оповещает подписчиков и отвечает
// актуальным представлением доски для участника.
func (s *Server) mutate(w http.ResponseWriter, r *http.Request, fn func(b *model.Board, pid string) error) {
	pid, err := participant(r)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	id := r.PathValue("id")
	b, err := s.store.Update(id, func(bd *model.Board) error { return fn(bd, pid) })
	if err != nil {
		s.writeErr(w, err)
		return
	}
	s.hub.Notify(id)
	b = s.syncTimer(b)
	writeJSON(w, http.StatusOK, b.View(pid))
}

// --- таймер фазы: автоматический переход на следующий этап ---

// syncTimer приводит планировщик в соответствие с доской: таймера нет —
// отменить запланированное; уже истёк — применить сейчас; идёт — назначить
// срабатывание. Возвращает доску после возможного применения.
func (s *Server) syncTimer(b *model.Board) *model.Board {
	if b.TimerEndsAt == nil {
		s.cancelTimer(b.ID)
		return b
	}
	if !time.Now().Before(*b.TimerEndsAt) {
		if nb := s.fireTimer(b.ID); nb != nil {
			return nb
		}
		return b
	}
	s.scheduleTimer(b.ID, *b.TimerEndsAt)
	return b
}

func (s *Server) scheduleTimer(id string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.timers[id]; ok {
		t.Stop()
	}
	s.timers[id] = time.AfterFunc(time.Until(at), func() { s.fireTimer(id) })
}

func (s *Server) cancelTimer(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.timers[id]; ok {
		t.Stop()
		delete(s.timers, id)
	}
}

// fireTimer применяет истечение таймера к доске. Если таймер к этому моменту
// снят или перенесён, Update откатывается и ничего не меняется.
func (s *Server) fireTimer(id string) *model.Board {
	s.cancelTimer(id)
	b, err := s.store.Update(id, func(bd *model.Board) error {
		if !bd.ExpireTimer(time.Now()) {
			return errTimerNotDue
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, errTimerNotDue) && !errors.Is(err, model.ErrNotFound) {
			s.log.Error("таймер фазы не применён", "board", id, "err", err)
		}
		return nil
	}
	s.log.Info("таймер истёк, этап переключён", "board", id, "phase", b.Phase)
	s.hub.Notify(id)
	return b
}

func (s *Server) handleTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, model.Templates)
}

func (s *Server) handleCreateBoard(w http.ResponseWriter, r *http.Request) {
	pid, err := participant(r)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	var in struct {
		Title    string `json:"title"`
		Template string `json:"template"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	b, err := model.NewBoard(in.Title, in.Template, pid)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if err := s.store.Create(b); err != nil {
		s.writeErr(w, err)
		return
	}
	s.log.Info("доска создана", "board", b.ID, "template", b.Template)
	writeJSON(w, http.StatusCreated, b.View(pid))
}

func (s *Server) handleGetBoard(w http.ResponseWriter, r *http.Request) {
	pid, err := participant(r)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	b, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	b = s.syncTimer(b) // после рестарта здесь таймер встаёт на место
	writeJSON(w, http.StatusOK, b.View(pid))
}

func (s *Server) handleAddCard(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ColumnID string `json:"columnId"`
		Text     string `json:"text"`
		Author   string `json:"author"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	s.mutate(w, r, func(b *model.Board, pid string) error {
		_, err := b.AddCard(in.ColumnID, in.Text, in.Author, pid)
		return err
	})
}

func (s *Server) handleEditCard(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	cardID := r.PathValue("cardId")
	s.mutate(w, r, func(b *model.Board, pid string) error {
		return b.EditCard(cardID, in.Text, pid)
	})
}

func (s *Server) handleDeleteCard(w http.ResponseWriter, r *http.Request) {
	cardID := r.PathValue("cardId")
	s.mutate(w, r, func(b *model.Board, pid string) error {
		return b.DeleteCard(cardID, pid)
	})
}

func (s *Server) handleVote(w http.ResponseWriter, r *http.Request) {
	cardID := r.PathValue("cardId")
	s.mutate(w, r, func(b *model.Board, pid string) error {
		_, err := b.ToggleVote(cardID, pid)
		return err
	})
}

func (s *Server) handleSetPhase(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Phase model.Phase `json:"phase"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	s.mutate(w, r, func(b *model.Board, pid string) error {
		return b.SetPhase(in.Phase, pid)
	})
}

func (s *Server) handleSetVoteLimit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		VoteLimit int `json:"voteLimit"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	s.mutate(w, r, func(b *model.Board, pid string) error {
		return b.SetVoteLimit(in.VoteLimit, pid)
	})
}

func (s *Server) handleGroupCard(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TargetCardID string `json:"targetCardId"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	cardID := r.PathValue("cardId")
	s.mutate(w, r, func(b *model.Board, pid string) error {
		return b.GroupCards(cardID, in.TargetCardID, pid)
	})
}

func (s *Server) handleUngroupCard(w http.ResponseWriter, r *http.Request) {
	cardID := r.PathValue("cardId")
	s.mutate(w, r, func(b *model.Board, pid string) error {
		return b.UngroupCard(cardID, pid)
	})
}

func (s *Server) handleAddAction(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text     string `json:"text"`
		Assignee string `json:"assignee"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	s.mutate(w, r, func(b *model.Board, pid string) error {
		_, err := b.AddAction(in.Text, in.Assignee, pid)
		return err
	})
}

func (s *Server) handleEditAction(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text     string `json:"text"`
		Assignee string `json:"assignee"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	actionID := r.PathValue("actionId")
	s.mutate(w, r, func(b *model.Board, pid string) error {
		return b.EditAction(actionID, in.Text, in.Assignee, pid)
	})
}

func (s *Server) handleDeleteAction(w http.ResponseWriter, r *http.Request) {
	actionID := r.PathValue("actionId")
	s.mutate(w, r, func(b *model.Board, pid string) error {
		return b.DeleteAction(actionID, pid)
	})
}

func (s *Server) handleToggleActionDone(w http.ResponseWriter, r *http.Request) {
	actionID := r.PathValue("actionId")
	s.mutate(w, r, func(b *model.Board, pid string) error {
		_, err := b.ToggleActionDone(actionID, pid)
		return err
	})
}

// handleNextBoard создаёт следующее ретро из текущего. Сначала в предыдущей
// доске резервируется NextID (проверка прав и «следующего ещё нет» идёт под
// её мьютексом), затем создаётся новая доска; если создать не удалось,
// NextID снимается.
func (s *Server) handleNextBoard(w http.ResponseWriter, r *http.Request) {
	pid, err := participant(r)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	var in struct {
		Title string `json:"title"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	prevID := r.PathValue("id")
	var next *model.Board
	if _, err := s.store.Update(prevID, func(prev *model.Board) error {
		b, err := prev.NextBoard(in.Title, pid)
		if err != nil {
			return err
		}
		prev.NextID = b.ID
		next = b
		return nil
	}); err != nil {
		s.writeErr(w, err)
		return
	}
	if err := s.store.Create(next); err != nil {
		_, _ = s.store.Update(prevID, func(prev *model.Board) error {
			prev.NextID = ""
			return nil
		})
		s.writeErr(w, err)
		return
	}
	s.hub.Notify(prevID)
	s.log.Info("создано следующее ретро", "board", next.ID, "prev", prevID)
	writeJSON(w, http.StatusCreated, next.View(pid))
}

// historyItem — одно ретро в цепочке команды.
type historyItem struct {
	ID          string      `json:"id"`
	Title       string      `json:"title"`
	Phase       model.Phase `json:"phase"`
	CreatedAt   time.Time   `json:"createdAt"`
	OpenActions int         `json:"openActions"`
	Current     bool        `json:"current"`
}

// handleHistory отдаёт цепочку ретро, в которую входит доска, от первого к
// последнему. Длина ограничена, чтобы повреждённые ссылки не зациклили обход.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, err := s.store.Get(id)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	const limit = 200
	item := func(b *model.Board) historyItem {
		open := 0
		for _, a := range b.Actions {
			if !a.Done {
				open++
			}
		}
		return historyItem{ID: b.ID, Title: b.Title, Phase: b.Phase, CreatedAt: b.CreatedAt, OpenActions: open, Current: b.ID == id}
	}
	var back []historyItem
	for b, n := cur, 0; b.PrevID != "" && n < limit; n++ {
		p, err := s.store.Get(b.PrevID)
		if err != nil {
			break
		}
		back = append(back, item(p))
		b = p
	}
	chain := make([]historyItem, 0, len(back)+1)
	for i := len(back) - 1; i >= 0; i-- {
		chain = append(chain, back[i])
	}
	chain = append(chain, item(cur))
	for b, n := cur, 0; b.NextID != "" && n < limit; n++ {
		nx, err := s.store.Get(b.NextID)
		if err != nil {
			break
		}
		chain = append(chain, item(nx))
		b = nx
	}
	writeJSON(w, http.StatusOK, chain)
}

func (s *Server) handleSetTimer(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Minutes int `json:"minutes"`
	}
	if err := decode(w, r, &in); err != nil {
		s.writeErr(w, err)
		return
	}
	s.mutate(w, r, func(b *model.Board, pid string) error {
		return b.SetTimer(in.Minutes, time.Now(), pid)
	})
}

// handleEvents — SSE-поток. На каждое изменение доски (и на вход/выход
// участников) клиент получает событие board с полным представлением доски.
// Доски маленькие, поэтому проще слать всё целиком, чем дифф.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	pid, err := participant(r)
	if err != nil {
		s.writeErr(w, err)
		return
	}
	id := r.PathValue("id")
	if _, err := s.store.Get(id); err != nil {
		s.writeErr(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // nginx: не буферизовать поток
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	signals, unsubscribe := s.hub.Subscribe(id)
	defer func() {
		unsubscribe()
		s.hub.Notify(id) // у остальных обновится счётчик онлайн
	}()
	s.hub.Notify(id) // первое событие нам и обновление счётчика остальным

	send := func() bool {
		b, err := s.store.Get(id)
		if err != nil {
			return false
		}
		b = s.syncTimer(b)
		payload := struct {
			model.BoardView
			Online int `json:"online"`
		}{b.View(pid), s.hub.Count(id)}
		data, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: board\ndata: %s\n\n", data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.hub.Done():
			return
		case <-signals:
			if !send() {
				return
			}
		case <-heartbeat.C:
			// Комментарий держит соединение живым через прокси и балансировщики.
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	b, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		s.writeErr(w, err)
		return
	}
	if b.Phase == model.PhaseWrite {
		s.writeErr(w, fmt.Errorf("%w: экспорт доступен после показа карточек", model.ErrPhase))
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="retro-`+b.ID+`.md"`)
	_, _ = io.WriteString(w, b.Markdown())
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(s.index)
}

// withLogging пишет одну строку на запрос. ResponseWriter не оборачивается:
// обёртка спрятала бы http.Flusher, без которого SSE не работает.
func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/static/") {
			return
		}
		s.log.Info("http", "method", r.Method, "path", r.URL.Path, "dur", time.Since(start).Round(time.Millisecond))
	})
}
