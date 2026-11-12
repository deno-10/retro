package model

import "fmt"

// JiraExport сохраняется до обращения к Jira. Неопределённый результат нельзя
// повторно отправлять: Jira могла создать задачу даже без ответа клиенту.
type JiraExport struct {
	Status    string `json:"status"`
	Label     string `json:"label"`
	Target    string `json:"target"`
	Key       string `json:"key,omitempty"`
	URL       string `json:"url,omitempty"`
	SearchURL string `json:"searchUrl"`
	Error     string `json:"error,omitempty"`
}

type JiraView struct {
	Status    string `json:"status"`
	Key       string `json:"key,omitempty"`
	URL       string `json:"url,omitempty"`
	SearchURL string `json:"searchUrl,omitempty"`
	Error     string `json:"error,omitempty"`
}

type JiraConfigView struct {
	Project   string `json:"project"`
	IssueType string `json:"issueType"`
}

func (a *Action) JiraUnresolved() bool {
	return a.Jira != nil && (a.Jira.Status == "pending" || a.Jira.Status == "unknown")
}

// BeginJira резервирует одну отправку атомарно с решением. Повторный запрос
// возвращает сохранённое состояние без нового обращения на создание задачи.
func (b *Board) BeginJira(id, actor string, revision uint64, target, label, searchURL string) (*Action, bool, error) {
	if actor != b.OwnerID {
		return nil, false, ErrForbidden
	}
	a := b.findAction(id)
	if a == nil {
		return nil, false, ErrNotFound
	}
	if a.Jira != nil && a.Jira.Status != "failed" {
		return a, false, nil
	}
	if b.Phase != PhaseDiscuss {
		return nil, false, ErrPhase
	}
	if b.NextID != "" {
		return nil, false, fmt.Errorf("%w: создавайте задачи из последнего ретро команды", ErrConflict)
	}
	if a.Revision != revision {
		return nil, false, ErrConflict
	}
	a.Jira = &JiraExport{Status: "pending", Target: target, Label: label, SearchURL: searchURL}
	a.Revision++
	return a, true, nil
}

// JiraForCheck разрешает только поиск результата уже начатой отправки.
func (b *Board) JiraForCheck(id, actor, target string) (*JiraExport, error) {
	if actor != b.OwnerID {
		return nil, ErrForbidden
	}
	a := b.findAction(id)
	if a == nil {
		return nil, ErrNotFound
	}
	if a.Jira == nil {
		return nil, fmt.Errorf("%w: отправка в Jira ещё не начиналась", ErrInvalid)
	}
	if a.Jira.Target != target {
		return nil, fmt.Errorf("%w: настройки Jira изменились; для проверки восстановите прежнее подключение", ErrConflict)
	}
	return a.Jira, nil
}

func (b *Board) FinishJira(id, label, status, key, url, message string) (bool, error) {
	a := b.findAction(id)
	if a == nil {
		return false, ErrNotFound
	}
	if a.Jira == nil || a.Jira.Label != label {
		return false, ErrConflict
	}
	if a.Jira.Status == "created" {
		return false, nil // проверка могла найти задачу раньше ответа создания
	}
	if status != "created" && status != "failed" && status != "unknown" {
		return false, ErrInvalid
	}
	a.Jira.Status, a.Jira.Key, a.Jira.URL, a.Jira.Error = status, key, url, message
	a.Revision++
	return true, nil
}
