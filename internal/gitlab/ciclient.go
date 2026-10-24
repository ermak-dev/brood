package gitlab

import (
	"fmt"
	"net/url"
	"strconv"
)

// CI-запросы: пайплайны, джобы, раннеры.

// Pipelines — последние n пайплайнов проекта, новые первыми.
func (c *Client) Pipelines(n int) ([]Pipeline, error) {
	raw, err := c.call("GET", "/pipelines", nil, url.Values{
		"order_by": {"id"}, "sort": {"desc"}, "per_page": {fmt.Sprint(n)},
	})
	if err != nil {
		return nil, err
	}
	items, _ := raw.([]any)
	out := make([]Pipeline, len(items))
	for i, item := range items {
		out[i] = Pipeline(item.(map[string]any))
	}
	return out, nil
}

// PipelineByID — пайплайн целиком: yaml_errors есть только здесь, не в
// списке.
func (c *Client) PipelineByID(pid int) (Pipeline, error) {
	raw, err := c.call("GET", "/pipelines/"+strconv.Itoa(pid), nil, nil)
	if err != nil {
		return nil, err
	}
	return Pipeline(raw.(map[string]any)), nil
}

// Jobs — джобы пайплайна в порядке возрастания id (переключённые копии
// GitLab сам прячет: показывается последняя попытка, та, что решает цвет).
func (c *Client) Jobs(pid int) ([]Job, error) {
	raw, err := c.paged("/pipelines/"+strconv.Itoa(pid)+"/jobs", nil)
	if err != nil {
		return nil, err
	}
	out := make([]Job, len(raw))
	for i, item := range raw {
		out[i] = Job(item.(map[string]any))
	}
	// Сортировка по id: стабильный порядок стадий.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].ID() < out[j-1].ID(); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// Job — карточка джобы.
func (c *Client) Job(jid int) (Job, error) {
	raw, err := c.call("GET", "/jobs/"+strconv.Itoa(jid), nil, nil)
	if err != nil {
		return nil, err
	}
	return Job(raw.(map[string]any)), nil
}

// Trace — лог джобы текстом, как его показал бы терминал: у GitLab это не
// JSON, а сырой вывод раннера с \r секций.
func (c *Client) Trace(jid int) (string, error) {
	raw, err := c.requestText("GET", c.base()+"/jobs/"+strconv.Itoa(jid)+"/trace",
		"/jobs/"+strconv.Itoa(jid)+"/trace")
	if err != nil {
		return "", err
	}
	return ScreenLines(raw), nil
}

// Lint — POST /ci/lint проекта: include в .gitlab-ci.yml разрешаются от него.
func (c *Client) Lint(content string) (map[string]any, error) {
	raw, err := c.call("POST", "/ci/lint", map[string]any{
		"content": content, "include_jobs": true,
	}, nil)
	if err != nil {
		return nil, err
	}
	return raw.(map[string]any), nil
}

// RetryJob перезапускает законченную джобу.
func (c *Client) RetryJob(jid int) (Job, error) {
	raw, err := c.call("POST", "/jobs/"+strconv.Itoa(jid)+"/retry", nil, nil)
	if err != nil {
		return nil, err
	}
	return Job(raw.(map[string]any)), nil
}

// RetryPipeline перезапускает провалившиеся джобы пайплайна.
func (c *Client) RetryPipeline(pid int) (Pipeline, error) {
	raw, err := c.call("POST", "/pipelines/"+strconv.Itoa(pid)+"/retry", nil, nil)
	if err != nil {
		return nil, err
	}
	return Pipeline(raw.(map[string]any)), nil
}

// CancelPipeline отменяет идущий пайплайн.
func (c *Client) CancelPipeline(pid int) error {
	_, err := c.call("POST", "/pipelines/"+strconv.Itoa(pid)+"/cancel", nil, nil)
	return err
}

// Runners — раннеры проекта с карточками: список метки не несёт, только
// карточка раннера, а она у GitLab не под проектом.
func (c *Client) Runners() ([]Runner, error) {
	ids, err := c.paged("/runners", nil)
	if err != nil {
		return nil, err
	}
	out := []Runner{}
	for _, item := range ids {
		m, _ := item.(map[string]any)
		id := 0
		if f, ok := m["id"].(float64); ok {
			id = int(f)
		}
		raw, err := c.callSite("GET", "/runners/"+strconv.Itoa(id), nil, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, Runner(raw.(map[string]any)))
	}
	return out, nil
}

// RunningJobs — идущие сейчас джобы проекта, каждая — с раннером, который
// её держит.
func (c *Client) RunningJobs() ([]Job, error) {
	raw, err := c.paged("/jobs", url.Values{"scope[]": {"running"}})
	if err != nil {
		return nil, err
	}
	out := make([]Job, len(raw))
	for i, item := range raw {
		out[i] = Job(item.(map[string]any))
	}
	return out, nil
}

// PipelineBySHA — последний пайплайн коммита либо nil. ref — только на
// этой ветке: у коммита бывают пайплайны разных веток, и ветка эпика,
// созданная на коммите main, иначе заслоняла бы пайплайн main своим.
func (c *Client) PipelineBySHA(sha, ref string) (Pipeline, error) {
	params := url.Values{"sha": {sha}, "order_by": {"id"}, "sort": {"desc"}, "per_page": {"1"}}
	if ref != "" {
		params.Set("ref", ref)
	}
	raw, err := c.call("GET", "/pipelines", nil, params)
	if err != nil {
		return nil, err
	}
	items, _ := raw.([]any)
	if len(items) == 0 {
		return nil, nil
	}
	return Pipeline(items[0].(map[string]any)), nil
}
