// Package jira обращается к REST API Jira. Учётные данные никогда не передаются браузеру.
package jira

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type Config struct {
	BaseURL, Deployment, Project, IssueType, AuthMode, User, Token, PublicURL string
	ExtraFields                                                               map[string]json.RawMessage
}

type Client struct {
	config Config
	http   *http.Client
}

type Input struct{ Text, Assignee, BoardID, BoardTitle, Label string }
type Issue struct {
	Key string `json:"key"`
	URL string `json:"-"`
}
type Failure struct {
	Message   string
	Uncertain bool
}

func (e *Failure) Error() string { return e.Message }

var projectPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
var typePattern = regexp.MustCompile(`^[0-9]{1,20}$`)
var keyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*-[1-9][0-9]*$`)
var labelPattern = regexp.MustCompile(`^retro-[a-f0-9]{16}$`)

// FromEnv не включает интеграцию без адреса. Частичная конфигурация — ошибка
// запуска, чтобы опечатка не выглядела как работающая интеграция.
func FromEnv() (*Client, error) {
	c := Config{BaseURL: os.Getenv("JIRA_BASE_URL"), Deployment: os.Getenv("JIRA_DEPLOYMENT"),
		Project: os.Getenv("JIRA_PROJECT_KEY"), IssueType: os.Getenv("JIRA_ISSUE_TYPE_ID"),
		AuthMode: os.Getenv("JIRA_AUTH_MODE"), User: os.Getenv("JIRA_USER"), Token: os.Getenv("JIRA_TOKEN"),
		PublicURL: os.Getenv("RETRO_PUBLIC_URL")}
	tokenFile := os.Getenv("JIRA_TOKEN_FILE")
	fields := os.Getenv("JIRA_EXTRA_FIELDS")
	if c.BaseURL == "" && c.Project == "" && c.IssueType == "" && c.Token == "" && tokenFile == "" && c.User == "" && fields == "" {
		return nil, nil
	}
	if tokenFile != "" {
		if c.Token != "" {
			return nil, errors.New("Jira: задайте только JIRA_TOKEN или JIRA_TOKEN_FILE")
		}
		data, err := os.ReadFile(tokenFile)
		if err != nil {
			return nil, errors.New("Jira: не удалось прочитать JIRA_TOKEN_FILE")
		}
		c.Token = strings.TrimSpace(string(data))
	}
	if fields != "" {
		if len(fields) > 16<<10 || json.Unmarshal([]byte(fields), &c.ExtraFields) != nil || c.ExtraFields == nil {
			return nil, errors.New("Jira: JIRA_EXTRA_FIELDS должен содержать JSON-объект размером до 16 КиБ")
		}
	}
	return New(c)
}

func validBase(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	// HTTP допустим только для локальных тестов/локального экземпляра Jira.
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))
}

func New(c Config) (*Client, error) {
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	c.PublicURL = strings.TrimRight(c.PublicURL, "/")
	if !validBase(c.BaseURL) {
		return nil, errors.New("Jira: нужен JIRA_BASE_URL с HTTPS, без логина, параметров и фрагмента")
	}
	if c.PublicURL != "" && !validBase(c.PublicURL) {
		return nil, errors.New("Jira: неверный RETRO_PUBLIC_URL")
	}
	if !projectPattern.MatchString(c.Project) || !typePattern.MatchString(c.IssueType) {
		return nil, errors.New("Jira: укажите JIRA_PROJECT_KEY и числовой JIRA_ISSUE_TYPE_ID")
	}
	if c.Deployment == "" {
		c.Deployment = "datacenter"
	}
	if c.Deployment != "datacenter" && c.Deployment != "cloud" {
		return nil, errors.New("Jira: JIRA_DEPLOYMENT должен быть datacenter или cloud")
	}
	if c.AuthMode == "" {
		c.AuthMode = "bearer"
	}
	if c.AuthMode != "bearer" && c.AuthMode != "basic" {
		return nil, errors.New("Jira: JIRA_AUTH_MODE должен быть bearer или basic")
	}
	if c.Token == "" || strings.ContainsAny(c.Token, "\r\n") || (c.AuthMode == "basic" && (c.User == "" || strings.ContainsAny(c.User, ":\r\n"))) {
		return nil, errors.New("Jira: нужен токен; для basic также укажите JIRA_USER")
	}
	for _, field := range []string{"project", "issuetype", "summary", "description", "labels", "assignee"} {
		if _, ok := c.ExtraFields[field]; ok {
			return nil, fmt.Errorf("Jira: поле %s нельзя переопределять в JIRA_EXTRA_FIELDS", field)
		}
	}
	return &Client{config: c, http: &http.Client{Timeout: 7 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (c *Client) Project() string   { return c.config.Project }
func (c *Client) IssueType() string { return c.config.IssueType }
func (c *Client) Target() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(c.config.BaseURL+"\n"+c.config.Project+"\n"+c.config.IssueType+"\n"+c.config.Deployment)))
}
func (c *Client) query(label string) string {
	return fmt.Sprintf(`project = "%s" AND labels = "%s"`, c.config.Project, label)
}
func (c *Client) SearchURL(label string) string {
	return c.config.BaseURL + "/issues/?jql=" + url.QueryEscape(c.query(label))
}

// do намеренно не повторяет POST и не следует редиректам. Текст ошибок Jira
// может содержать данные запроса, поэтому наружу выходят только наши сообщения.
func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.config.BaseURL+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if c.config.AuthMode == "basic" {
		req.SetBasicAuth(c.config.User, c.config.Token)
	} else {
		req.Header.Set("Authorization", "Bearer "+c.config.Token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, errors.New("Jira не ответила; проверьте подключение сервера")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
	if err != nil || len(data) > 256<<10 {
		return nil, resp.StatusCode, errors.New("не удалось прочитать ответ Jira")
	}
	return data, resp.StatusCode, nil
}

func (c *Client) issue(key string) (Issue, error) {
	if !keyPattern.MatchString(key) || !strings.HasPrefix(key, c.config.Project+"-") {
		return Issue{}, errors.New("Jira вернула некорректный ключ задачи")
	}
	return Issue{Key: key, URL: c.config.BaseURL + "/browse/" + key}, nil
}

func (c *Client) Create(ctx context.Context, in Input) (Issue, *Failure) {
	if !labelPattern.MatchString(in.Label) {
		return Issue{}, &Failure{Message: "Некорректная метка отправки"}
	}
	summary := []rune(strings.Join(strings.Fields(in.Text), " "))
	if len(summary) > 240 {
		summary = append(summary[:239], '…')
	}
	description := in.Text + "\n\nРетро: " + in.BoardTitle
	if in.Assignee != "" {
		description += "\nОтветственный в Retro: " + in.Assignee
	}
	if c.config.PublicURL != "" {
		description += "\nИсточник: " + c.config.PublicURL + "/b/" + url.PathEscape(in.BoardID)
	}
	description += "\nМетка отправки: " + in.Label
	fields := map[string]any{}
	for key, value := range c.config.ExtraFields {
		fields[key] = value
	}
	fields["project"] = map[string]string{"key": c.config.Project}
	fields["issuetype"] = map[string]string{"id": c.config.IssueType}
	fields["summary"], fields["description"] = string(summary), description
	fields["labels"] = []string{"retro", in.Label}
	// В некоторых проектах reporter обязателен и не имеет значения по
	// умолчанию. Автором задачи становится учётная запись интеграции.
	if _, configured := fields["reporter"]; !configured {
		data, status, err := c.do(ctx, http.MethodGet, "/rest/api/2/myself", nil)
		var user struct {
			Name      string `json:"name"`
			AccountID string `json:"accountId"`
		}
		if err != nil || status != 200 || json.Unmarshal(data, &user) != nil {
			return Issue{}, &Failure{Message: "Не удалось определить автора задачи в Jira. Проверьте подключение и токен. Запрос создания не отправлялся."}
		}
		field, value := "name", user.Name
		if c.config.Deployment == "cloud" {
			field, value = "accountId", user.AccountID
		}
		if value == "" {
			return Issue{}, &Failure{Message: "Jira не вернула идентификатор автора. Проверьте тип подключения. Запрос создания не отправлялся."}
		}
		fields["reporter"] = map[string]string{field: value}
	}
	data, status, err := c.do(ctx, http.MethodPost, "/rest/api/2/issue", map[string]any{"fields": fields})
	if err == nil && status == http.StatusCreated {
		var result Issue
		if json.Unmarshal(data, &result) == nil {
			if issue, err := c.issue(result.Key); err == nil {
				return issue, nil
			}
		}
	}
	// Только явный отказ до создания позволяет повторить отправку.
	switch status {
	case 400, 422:
		return Issue{}, &Failure{Message: "Jira отклонила поля задачи. Проверьте тип задачи, доступность поля labels и обязательные поля проекта."}
	case 401, 403:
		return Issue{}, &Failure{Message: "Jira отказала в доступе. Проверьте токен и права на создание задач в проекте."}
	case 404:
		return Issue{}, &Failure{Message: "Jira не нашла адрес API или проект. Проверьте настройки подключения."}
	}
	return Issue{}, &Failure{Message: "Результат отправки неизвестен. Проверьте Jira перед дальнейшими действиями; повторное создание заблокировано.", Uncertain: true}
}

// Find ищет только ранее отправленную задачу. Отсутствие в поиске не доказывает
// отсутствие задачи: индекс Jira может обновляться с задержкой.
func (c *Client) Find(ctx context.Context, label string) (Issue, error) {
	if !labelPattern.MatchString(label) {
		return Issue{}, errors.New("некорректная метка отправки")
	}
	endpoint := "/rest/api/2/search"
	if c.config.Deployment == "cloud" {
		endpoint = "/rest/api/3/search/jql"
	}
	query := url.Values{"jql": {c.query(label)}, "maxResults": {"2"}, "fields": {"labels"}}
	data, status, err := c.do(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil || status != 200 {
		return Issue{}, errors.New("не удалось проверить Jira; проверьте подключение и права на просмотр задач")
	}
	var result struct {
		Issues []struct {
			Key    string `json:"key"`
			Fields struct {
				Labels []string `json:"labels"`
			} `json:"fields"`
		} `json:"issues"`
	}
	if json.Unmarshal(data, &result) != nil || result.Issues == nil {
		return Issue{}, errors.New("не удалось прочитать результаты поиска Jira")
	}
	if len(result.Issues) == 0 {
		return Issue{}, errors.New("задача пока не найдена. Повторите проверку позже: индекс Jira обновляется с задержкой. Новая задача не создавалась")
	}
	if len(result.Issues) != 1 {
		return Issue{}, errors.New("найдено несколько задач; проверьте их в Jira вручную")
	}
	found := result.Issues[0]
	for _, actual := range found.Fields.Labels {
		if actual == label {
			return c.issue(found.Key)
		}
	}
	return Issue{}, errors.New("метка найденной задачи не совпадает с отправкой из Retro")
}
