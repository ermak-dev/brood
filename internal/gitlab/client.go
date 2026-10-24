// Трекер проекта и CI: issues GitLab. Пакет возвращает данные и ничего не
// печатает — печатают командлеты internal/cli.
//
// Координаты берутся из origin рабочей копии (env.Site/Project), секрет —
// GITLAB_TOKEN из .env: личный токен с правом api, единственный credential
// трекера и CI.
package gitlab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"git.ermak.dev/ermak/brood/internal/env"
)

const timeout = 20 * time.Second

// Issue — задача трекера. Поля — как отдаёт GitLab, разбор ленивый:
// клиентам нужны разные подмножества, а лишние поля не стоят своих типов.
type Issue map[string]any

func (i Issue) IID() int         { return int(i["iid"].(float64)) }
func (i Issue) Title() string    { s, _ := i["title"].(string); return s }
func (i Issue) State() string    { s, _ := i["state"].(string); return s }
func (i Issue) WebURL() string   { s, _ := i["web_url"].(string); return s }
func (i Issue) Body() string     { s, _ := i["description"].(string); return s }
func (i Issue) Labels() []string { return StringSlice(i["labels"]) }

// Pipeline, Job, Runner — ответы CI API.
type Pipeline map[string]any
type Job map[string]any
type Runner map[string]any

func (p Pipeline) ID() int        { return int(p["id"].(float64)) }
func (p Pipeline) Status() string { s, _ := p["status"].(string); return s }
func (p Pipeline) WebURL() string { s, _ := p["web_url"].(string); return s }
func (j Job) ID() int             { return int(j["id"].(float64)) }
func (j Job) Status() string      { s, _ := j["status"].(string); return s }
func (j Job) Stage() string       { s, _ := j["stage"].(string); return s }
func (j Job) Name() string        { s, _ := j["name"].(string); return s }
func (r Runner) ID() int          { return int(r["id"].(float64)) }

// StringSlice — []any из JSON в []string; не-строки пропускаются.
func StringSlice(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// Client — API одного проекта GitLab.
type Client struct {
	site    string
	project string
	token   string
	http    *http.Client
}

// New собирает клиент из координат origin и токена .env.
func New() (*Client, error) {
	site, err := env.Site()
	if err != nil {
		return nil, err
	}
	project, err := env.Project()
	if err != nil {
		return nil, err
	}
	token := env.Get("GITLAB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("no GitLab token configured: set GITLAB_TOKEN with the api scope")
	}
	return &Client{site: site, project: project, token: token, http: &http.Client{Timeout: timeout}}, nil
}

// HasCredentials — есть ли чем представиться GitLab.
func HasCredentials() bool { return env.Get("GITLAB_TOKEN") != "" }

func (c *Client) base() string {
	return "https://" + c.site + "/api/v4/projects/" + url.PathEscape(c.project)
}

// call — запрос к API проекта; ответ — разобранный JSON.
func (c *Client) call(method, path string, payload any, params url.Values) (any, error) {
	return c.request(method, c.base()+path, path, payload, params)
}

// callSite — запрос к API инстанса, мимо проекта: карточка раннера у
// GitLab живёт не под проектом, хотя раннер проекту и назначен.
func (c *Client) callSite(method, path string, payload any, params url.Values) (any, error) {
	return c.request(method, "https://"+c.site+"/api/v4"+path, path, payload, params)
}

func (c *Client) request(method, full, path string, payload any, params url.Values) (any, error) {
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	if len(params) > 0 {
		full += "?" + params.Encode()
	}
	req, err := http.NewRequest(method, full, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s -> HTTP %d: %s", method, path, resp.StatusCode, snippet(raw))
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	return out, nil
}

func (c *Client) requestText(method, full, path string) (string, error) {
	req, err := http.NewRequest(method, full, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("PRIVATE-TOKEN", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s %s -> HTTP %d: %s", method, path, resp.StatusCode, snippet(raw))
	}
	return string(raw), nil
}

func snippet(raw []byte) string {
	if len(raw) > 300 {
		raw = raw[:300]
	}
	return string(raw)
}

// paged — постраничный обход: per_page=100, пока страница полная.
func (c *Client) paged(path string, params url.Values) ([]any, error) {
	out := []any{}
	for page := 1; ; page++ {
		q := url.Values{}
		for k, v := range params {
			q[k] = v
		}
		q.Set("per_page", "100")
		q.Set("page", fmt.Sprint(page))
		batch, err := c.call("GET", path, nil, q)
		if err != nil {
			return nil, err
		}
		items, _ := batch.([]any)
		out = append(out, items...)
		if len(items) < 100 {
			return out, nil
		}
	}
}

// Issues — задачи как данные, тяжёлые первыми: сначала sev, потом номер.
func (c *Client) Issues(state string, labels []string, search string) ([]Issue, error) {
	params := url.Values{"state": {state}, "order_by": {"created_at"}}
	if len(labels) > 0 {
		params.Set("labels", join(labels, ","))
	}
	if search != "" {
		params.Set("search", search)
	}
	raw, err := c.paged("/issues", params)
	if err != nil {
		return nil, err
	}
	out := make([]Issue, len(raw))
	for i, item := range raw {
		out[i] = Issue(item.(map[string]any))
	}
	SortIssues(out)
	return out, nil
}

func join(items []string, sep string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

// Issue — одна задача по iid.
func (c *Client) Issue(iid string) (Issue, error) {
	raw, err := c.call("GET", "/issues/"+iid, nil, nil)
	if err != nil {
		return nil, err
	}
	return Issue(raw.(map[string]any)), nil
}

// CreateIssue открывает задачу с телом и метками.
func (c *Client) CreateIssue(title, body string, labels []string) (Issue, error) {
	raw, err := c.call("POST", "/issues", map[string]any{
		"title":       title,
		"description": body,
		"labels":      join(labels, ","),
	}, nil)
	if err != nil {
		return nil, err
	}
	return Issue(raw.(map[string]any)), nil
}

// Comment добавляет комментарий.
func (c *Client) Comment(iid, body string) error {
	_, err := c.call("POST", "/issues/"+iid+"/notes", map[string]any{"body": body}, nil)
	return err
}

// Relabel заменяет метки задачи.
func (c *Client) Relabel(iid string, labels []string) error {
	_, err := c.call("PUT", "/issues/"+iid, map[string]any{"labels": join(labels, ",")}, nil)
	return err
}

// Close закрывает задачу с финальными метками.
func (c *Client) Close(iid string, labels []string) error {
	_, err := c.call("PUT", "/issues/"+iid, map[string]any{
		"labels":      join(labels, ","),
		"state_event": "close",
	}, nil)
	return err
}

// Reopen открывает закрытую задачу с метками.
func (c *Client) Reopen(iid string, labels []string) error {
	_, err := c.call("PUT", "/issues/"+iid, map[string]any{
		"labels":      join(labels, ","),
		"state_event": "reopen",
	}, nil)
	return err
}

// EditPayload — поля правки: только названные. Пустая правка — ошибка
// аргументов, а не «ничего не менять».
type EditPayload map[string]any

// Edit меняет заголовок и/или тело. Что не названо, не трогается.
func (c *Client) Edit(iid string, p EditPayload) error {
	if len(p) == 0 {
		return fmt.Errorf("nothing to edit: name --title or --body-file")
	}
	_, err := c.call("PUT", "/issues/"+iid, map[string]any(p), nil)
	return err
}
