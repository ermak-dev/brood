package gitlab

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultTimeout bounds a whole operation, including pagination/hydration.
	DefaultTimeout = 30 * time.Second
	// MaxTimeout is the largest configurable operation budget.
	MaxTimeout = 2 * time.Minute
	// MaxResponseBytes bounds each uncompressed JSON response.
	MaxResponseBytes = 4 << 20
	// MaxOperationBytes bounds the sum of bodies across pages and hydration.
	MaxOperationBytes = 16 << 20
	// MaxPages bounds list requests per operation, shared by builds, triggers
	// and the optional legacy-route probe (at most 100 entries per page).
	MaxPages = 10
	pageSize = 100
)

// Config contains no credential and supplies no default host or project.
type Config struct {
	// APIBase is an explicit HTTPS API root ending in /api/v4, optionally
	// below a self-managed GitLab path prefix. No userinfo/query/fragment.
	APIBase   string
	ProjectID int64
	// Timeout is zero for DefaultTimeout, otherwise positive up to MaxTimeout.
	Timeout time.Duration
}

// Client reads one fixed project. It is safe for concurrent use. It adds no
// application-level retries, does not log, follow redirects, use environment
// proxies or read credentials from Git, files or the environment. The standard
// transport may retry a GET on a lost idle connection. The zero value is unusable.
type Client struct {
	base      string
	projectID int64
	token     *secret
	http      *http.Client
	timeout   time.Duration
}

type secret string

func (secret) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, "<redacted>") }

func (Client) String() string     { return "gitlab.Client{redacted}" }
func (c Client) GoString() string { return c.String() }

// Format hides the token with every fmt verb. Where reflection bypasses
// methods, the token is held only through a pointer, never in a Client value.
func (c Client) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, c.String()) }

// ErrorKind classifies a refusal without retaining untrusted upstream text.
type ErrorKind string

const (
	Invalid   ErrorKind = "invalid"
	Transport ErrorKind = "transport"
	HTTP      ErrorKind = "http"
	Response  ErrorKind = "response"
	Limit     ErrorKind = "limit"
	Binding   ErrorKind = "binding"
	Timeout   ErrorKind = "timeout"
	Canceled  ErrorKind = "canceled"
)

// Error contains only a local class and an optional numeric HTTP status.
// Upstream bodies, headers, request URLs and original errors are discarded,
// not wrapped, so formatting, JSON and error traversal cannot reveal tokens.
type Error struct {
	Kind   ErrorKind
	Status int
}

func (e *Error) Error() string {
	reason := map[ErrorKind]string{
		Invalid: "invalid configuration or identifier", Transport: "transport failed",
		HTTP: "HTTP request refused", Response: "invalid or incomplete response",
		Limit: "read limit exceeded", Binding: "response does not match the project or object binding",
		Timeout: "operation timed out", Canceled: "operation canceled",
	}[e.Kind]
	if reason == "" {
		reason = "request failed"
	}
	if e.Status != 0 {
		return "gitlab: " + reason + " (status " + strconv.Itoa(e.Status) + ")"
	}
	return "gitlab: " + reason
}

// Is retains cancellation semantics without exposing a transport error tree.
func (e *Error) Is(target error) bool {
	return e.Kind == Timeout && target == context.DeadlineExceeded || e.Kind == Canceled && target == context.Canceled
}
func fail(kind ErrorKind) error { return &Error{Kind: kind} }

// NewClient validates a separate, caller-supplied API token and fixed binding
// without contacting GitLab. A Git transport token is never a fallback.
func NewClient(token string, cfg Config) (*Client, error) {
	if len(token) == 0 || len(token) > 4096 {
		return nil, fail(Invalid)
	}
	for i := range len(token) {
		if token[i] < 0x21 || token[i] > 0x7e {
			return nil, fail(Invalid)
		}
	}
	if cfg.ProjectID <= 0 || cfg.Timeout < 0 || cfg.Timeout > MaxTimeout || strings.Contains(cfg.APIBase, token) {
		return nil, fail(Invalid)
	}
	u, err := url.Parse(cfg.APIBase)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || strings.ContainsAny(cfg.APIBase, "#\\ \t\r\n") {
		return nil, fail(Invalid)
	}
	if strings.HasSuffix(u.Host, ":") {
		return nil, fail(Invalid)
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return nil, fail(Invalid)
		}
	}
	path := strings.TrimSuffix(u.Path, "/")
	if !strings.HasSuffix(path, "/api/v4") {
		return nil, fail(Invalid)
	}
	for _, segment := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return nil, fail(Invalid)
		}
		for _, ch := range segment {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.ContainsRune("._~-", ch)) {
				return nil, fail(Invalid)
			}
		}
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    10 * time.Second,
		MaxResponseHeaderBytes: 32 << 10,
		MaxIdleConns:           2, MaxIdleConnsPerHost: 2, MaxConnsPerHost: 4,
		IdleConnTimeout:    90 * time.Second,
		DisableCompression: true,
	}
	held := secret(token)
	return &Client{base: u.Scheme + "://" + u.Host + path, projectID: cfg.ProjectID, token: &held, timeout: timeout, http: &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// Close releases idle connections without interrupting in-flight operations.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// get accepts only package-owned constant paths and decimal identifiers.
// Query values are encoded, never interpolated. Neither response URLs nor
// Link/Location headers are request targets.
func (c *Client) get(ctx context.Context, remaining *int, path string, query url.Values, out any) (pageMetadata, error) {
	if ctx.Err() != nil {
		return pageMetadata{}, contextFailure(ctx)
	}
	target := c.base + "/projects/" + strconv.FormatInt(c.projectID, 10) + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return pageMetadata{}, fail(Invalid)
	}
	req.Header.Set("PRIVATE-TOKEN", string(*c.token))
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return pageMetadata{}, contextFailure(ctx)
		}
		return pageMetadata{}, fail(Transport)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return pageMetadata{}, &Error{Kind: HTTP, Status: resp.StatusCode}
	}
	bound := min(MaxResponseBytes, *remaining)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(bound)+1))
	if ctx.Err() != nil {
		return pageMetadata{}, contextFailure(ctx)
	}
	if err != nil {
		return pageMetadata{}, fail(Response)
	}
	if len(raw) > bound {
		return pageMetadata{}, fail(Limit)
	}
	*remaining -= len(raw)
	var decodeErr error
	if page, ok := out.(interface{ decodeJSON([]byte) error }); ok {
		decodeErr = page.decodeJSON(raw)
	} else {
		decodeErr = json.Unmarshal(raw, out)
	}
	if decodeErr != nil {
		return pageMetadata{}, fail(Response)
	}
	if ctx.Err() != nil {
		return pageMetadata{}, contextFailure(ctx)
	}
	if query.Get("page") != "" {
		return paginationMetadata(resp.Header, req.URL)
	}
	return pageMetadata{}, nil
}

// paginationNext accepts only the next locally constructed request, never a
// target chosen by GitLab. Link is evidence of another page, not a URL to fetch.
func paginationNext(header http.Header, current *url.URL) (string, error) {
	meta, err := paginationMetadata(header, current)
	return meta.next, err
}

type pageMetadata struct {
	next    string
	last    int
	hasLast bool
	total   int // -1 when X-Total is absent
}

func paginationMetadata(header http.Header, current *url.URL) (pageMetadata, error) {
	invalid := func() (pageMetadata, error) { return pageMetadata{}, fail(Response) }
	query := current.Query()
	page, _ := strconv.Atoi(query.Get("page"))
	// Empty next/prev headers denote boundaries, not missing evidence.
	x := make(map[string]int)
	for _, key := range []string{"X-Page", "X-Per-Page", "X-Next-Page", "X-Prev-Page", "X-Total-Pages", "X-Total"} {
		values := header.Values(key)
		if len(values) == 0 {
			continue
		}
		if len(values) != 1 {
			return invalid()
		}
		n := 0
		if values[0] != "" || key != "X-Next-Page" && key != "X-Prev-Page" {
			var err error
			n, err = strconv.Atoi(values[0])
			if err != nil || n < 0 || strconv.Itoa(n) != values[0] || n == 0 && (key == "X-Next-Page" || key == "X-Prev-Page") {
				return invalid()
			}
		}
		x[key] = n
	}
	for key, want := range map[string]int{"X-Page": page, "X-Per-Page": pageSize, "X-Prev-Page": page - 1} {
		if got, ok := x[key]; ok && got != want {
			return invalid()
		}
	}
	if next, ok := x["X-Next-Page"]; ok && next != 0 && next != page+1 {
		return invalid()
	}
	relations := make(map[string]int)
	for _, value := range header.Values("Link") {
		for _, entry := range strings.Split(value, ",") {
			parts := strings.Split(entry, ";")
			target := strings.TrimSpace(parts[0])
			if !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
				return invalid()
			}
			rels, err := paginationRelation(parts[1:])
			if err != nil {
				return invalid()
			}
			u, err := url.Parse(target[1 : len(target)-1])
			if err != nil || u.Scheme != current.Scheme || u.Host != current.Host || u.User != nil || u.Opaque != "" || u.EscapedPath() != current.EscapedPath() || strings.Contains(target, "#") {
				return invalid()
			}
			q, err := url.ParseQuery(u.RawQuery)
			n, numErr := strconv.Atoi(q.Get("page"))
			if err != nil || numErr != nil || n <= 0 || q.Get("page") != strconv.Itoa(n) {
				return invalid()
			}
			query.Set("page", strconv.Itoa(n))
			if q.Encode() != query.Encode() {
				return invalid()
			}
			for _, rel := range rels {
				if _, exists := relations[rel]; exists {
					return invalid()
				}
				switch rel {
				case "first":
					if n != 1 {
						return invalid()
					}
				case "prev":
					if page == 1 || n != page-1 {
						return invalid()
					}
				case "next":
					if n != page+1 {
						return invalid()
					}
				case "last":
					if n < page {
						return invalid()
					}
				}
				relations[rel] = n
			}
		}
	}
	meta := pageMetadata{last: relations["last"], total: -1}
	meta.hasLast = meta.last != 0
	if total, ok := x["X-Total-Pages"]; ok {
		if total < page && !(total == 0 && page == 1) || meta.last != 0 && total != meta.last {
			return invalid()
		}
		meta.last, meta.hasLast = total, true
	}
	if total, ok := x["X-Total"]; ok {
		// Division avoids overflow even for a malicious maximum-sized count.
		pages := total / pageSize
		if total%pageSize != 0 {
			pages++
		}
		if known, ok := x["X-Total-Pages"]; ok && pages != known || meta.last != 0 && pages != meta.last || pages < page && !(pages == 0 && page == 1) {
			return invalid()
		}
		meta.last, meta.hasLast, meta.total = pages, true, total
	}
	next := relations["next"]
	if n, ok := x["X-Next-Page"]; ok {
		if next != 0 && n != next {
			return invalid()
		}
		next = n
	}
	if meta.last > page {
		// Do not silently finish a short page when last proves unread pages.
		// Refuse instead of synthesizing navigation from incomplete metadata.
		if next == 0 {
			return invalid()
		}
	} else if meta.hasLast {
		if next != 0 {
			return invalid()
		}
	}
	if next != 0 {
		meta.next = strconv.Itoa(next)
	}
	return meta, nil
}

// paginationRelation accepts a strict subset of RFC 8288: one rel parameter,
// containing only registered pagination relations. Extension parameters, quoted
// delimiters/escapes and duplicate parameters refuse the read, never hide next.
func paginationRelation(parts []string) ([]string, error) {
	if len(parts) != 1 {
		return nil, fail(Response)
	}
	key, value, ok := strings.Cut(parts[0], "=")
	if !ok || !strings.EqualFold(strings.Trim(key, " \t"), "rel") {
		return nil, fail(Response)
	}
	value = strings.Trim(value, " \t")
	quoted := len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"'
	if quoted {
		value = value[1 : len(value)-1]
	}
	relations := strings.Fields(value)
	if len(relations) == 0 || !quoted && len(relations) != 1 {
		return nil, fail(Response)
	}
	for i, relation := range relations {
		relations[i] = strings.ToLower(relation)
		switch relations[i] {
		case "next", "prev", "first", "last":
		default:
			return nil, fail(Response)
		}
	}
	return relations, nil
}

func contextFailure(ctx context.Context) error {
	if ctx.Err() == context.DeadlineExceeded {
		return fail(Timeout)
	}
	return fail(Canceled)
}
func decimal(id int64) string { return strconv.FormatInt(id, 10) }

// Project reads metadata and confirms the configured numeric project ID.
func (c *Client) Project(ctx context.Context) (Project, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	remaining := MaxOperationBytes
	var p Project
	if _, err := c.get(ctx, &remaining, "", nil, &p); err != nil {
		return Project{}, err
	}
	if p.ID != c.projectID {
		return Project{}, fail(Binding)
	}
	c.scrub(&p)
	return p, nil
}

// Issue reads an issue by positive project-local IID.
func (c *Client) Issue(ctx context.Context, iid int64) (Issue, error) {
	if iid <= 0 {
		return Issue{}, fail(Invalid)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	remaining := MaxOperationBytes
	var i Issue
	if _, err := c.get(ctx, &remaining, "/issues/"+decimal(iid), nil, &i); err != nil {
		return Issue{}, err
	}
	if i.ID <= 0 || i.IID != iid || i.ProjectID != c.projectID {
		return Issue{}, fail(Binding)
	}
	c.scrub(&i)
	return i, nil
}

// MergeRequest reads a positive IID. Source, target and MR project must all
// equal the binding; fork MRs are deliberately outside this first layer.
func (c *Client) MergeRequest(ctx context.Context, iid int64) (MergeRequest, error) {
	if iid <= 0 {
		return MergeRequest{}, fail(Invalid)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	remaining := MaxOperationBytes
	return c.mergeRequest(ctx, &remaining, iid)
}

func (c *Client) mergeRequest(ctx context.Context, remaining *int, iid int64) (MergeRequest, error) {
	var mr MergeRequest
	if _, err := c.get(ctx, remaining, "/merge_requests/"+decimal(iid), nil, &mr); err != nil {
		return MergeRequest{}, err
	}
	if mr.ID <= 0 || mr.IID != iid || mr.ProjectID != c.projectID || mr.SourceProjectID != c.projectID || mr.TargetProjectID != c.projectID {
		return MergeRequest{}, fail(Binding)
	}
	c.scrub(&mr)
	return mr, nil
}

// Pipeline reads a positive global pipeline ID under the bound project.
func (c *Client) Pipeline(ctx context.Context, id int64) (Pipeline, error) {
	if id <= 0 {
		return Pipeline{}, fail(Invalid)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	remaining := MaxOperationBytes
	return c.pipeline(ctx, &remaining, id)
}

func (c *Client) pipeline(ctx context.Context, remaining *int, id int64) (Pipeline, error) {
	var p Pipeline
	if _, err := c.get(ctx, remaining, "/pipelines/"+decimal(id), nil, &p); err != nil {
		return Pipeline{}, err
	}
	if p.ID != id || p.ProjectID != c.projectID {
		return Pipeline{}, fail(Binding)
	}
	c.scrub(&p)
	return p, nil
}

type pipelineReference struct {
	ID        int64 `json:"id"`
	ProjectID int64 `json:"project_id"`
}

// MergeRequestPipelines first checks the same-project MR, then lists its
// pipelines and hydrates each through the project pipeline endpoint. GitLab's
// abbreviated MR list can omit project_id, so it is not proof of binding.
// Every request shares one budget; any failure returns no partial results.
func (c *Client) MergeRequestPipelines(ctx context.Context, iid int64) ([]Pipeline, error) {
	if iid <= 0 {
		return nil, fail(Invalid)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	remaining := MaxOperationBytes
	if _, err := c.mergeRequest(ctx, &remaining, iid); err != nil {
		return nil, err
	}
	pages := MaxPages
	refs, err := readPages[pipelineReference](c, ctx, &remaining, &pages, "/merge_requests/"+decimal(iid)+"/pipelines", nil, func(p pipelineReference) int64 { return p.ID })
	if err != nil {
		return nil, err
	}
	out := make([]Pipeline, 0, len(refs))
	for _, ref := range refs {
		if ref.ProjectID != 0 && ref.ProjectID != c.projectID {
			return nil, fail(Binding)
		}
		p, err := c.pipeline(ctx, &remaining, ref.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Jobs lists current build and trigger jobs of a positive pipeline ID.
// Every row must report this exact project and pipeline. Trigger jobs use
// trigger_jobs, falling back to bridges only for a first-page 404 (pre-19.2).
// All reads, including that compatibility probe, share page/byte/time budgets.
// Downstream pipelines are not traversed. Any failure returns nil, never a
// build-only prefix that could be mistaken for complete CI evidence.
func (c *Client) Jobs(ctx context.Context, id int64) ([]Job, error) {
	if id <= 0 {
		return nil, fail(Invalid)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	remaining, pages := MaxOperationBytes, MaxPages
	path := "/pipelines/" + decimal(id)
	jobs, err := readPages[Job](c, ctx, &remaining, &pages, path+"/jobs", url.Values{"include_retried": {"false"}}, func(j Job) int64 { return j.ID })
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]bool)
	validate := func(j Job) error {
		if j.Pipeline.ID != id || j.Pipeline.ProjectID != c.projectID {
			return fail(Binding)
		}
		if seen[j.ID] {
			return fail(Response)
		}
		seen[j.ID] = true
		return nil
	}
	for _, j := range jobs {
		if err := validate(j); err != nil {
			return nil, err
		}
	}
	before := pages
	bridges, err := readPages[Bridge](c, ctx, &remaining, &pages, path+"/trigger_jobs", nil, func(b Bridge) int64 { return b.ID })
	if api, ok := err.(*Error); ok && api.Kind == HTTP && api.Status == http.StatusNotFound && before-pages == 1 {
		// An old route and a hidden/inaccessible route can both return 404.
		// Only a fully successful, bound legacy read permits any result.
		bridges, err = readPages[Bridge](c, ctx, &remaining, &pages, path+"/bridges", nil, func(b Bridge) int64 { return b.ID })
	}
	if err != nil {
		return nil, err
	}
	for _, b := range bridges {
		j := Job(b)
		if err := validate(j); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	for i := range jobs {
		c.scrub(&jobs[i])
	}
	return jobs, nil
}

type boundedPage[T any] []T

// decodeJSON validates a complete array, but stops before decoding a 101st row.
// Body bytes have already been bounded by get; the row limit also bounds DTO
// allocation. Do not wrap this in json.Unmarshal: that scans the whole array
// before invoking a custom unmarshaler, including an already-refused tail.
func (rows *boundedPage[T]) decodeJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return fail(Response)
	}
	*rows = make(boundedPage[T], 0)
	for decoder.More() {
		if len(*rows) == pageSize {
			return fail(Response)
		}
		var row T
		if decoder.Decode(&row) != nil {
			return fail(Response)
		}
		*rows = append(*rows, row)
	}
	token, err = decoder.Token()
	if err != nil || token != json.Delim(']') {
		return fail(Response)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fail(Response)
	}
	return nil
}

type paginationState struct {
	counts []int
	last   int // -1 до первого объявления последней страницы
	total  int // -1 до первого X-Total
}

// accept revalidates every page count when last/total evidence arrives.
// This checks offset response consistency, not an atomic snapshot.
func (s *paginationState) accept(count int, meta pageMetadata) (bool, error) {
	if meta.hasLast {
		if s.last >= 0 && s.last != meta.last {
			return false, fail(Response)
		}
		s.last = meta.last
	}
	if meta.total >= 0 {
		if s.total >= 0 && s.total != meta.total {
			return false, fail(Response)
		}
		s.total = meta.total
	}
	s.counts = append(s.counts, count)
	page := len(s.counts)
	if s.last == 0 {
		if page != 1 || count != 0 || meta.next != "" {
			return false, fail(Response)
		}
	} else if s.last > 0 {
		if page > s.last || page == s.last && meta.next != "" || page < s.last && meta.next == "" {
			return false, fail(Response)
		}
	}
	collected := 0
	for i, n := range s.counts {
		p := i + 1
		if s.last > 0 && (p < s.last && n != pageSize || p == s.last && p > 1 && n == 0) {
			return false, fail(Response)
		}
		collected += n
	}
	if s.total >= 0 && collected != min(s.total, page*pageSize) {
		return false, fail(Response)
	}
	return meta.next == "" && (count < pageSize || page == s.last), nil
}

// readPages constructs every numeric page locally. A full page without proof
// of the last page still needs probing: GitLab can omit pagination headers.
// Duplicate IDs (including concurrent offset drift) refuse the entire read.
func readPages[T any](c *Client, ctx context.Context, remaining, pages *int, path string, query url.Values, id func(T) int64) ([]T, error) {
	if query == nil {
		query = make(url.Values)
	}
	query.Set("per_page", strconv.Itoa(pageSize))
	out := make([]T, 0)
	seen := make(map[int64]bool)
	state := paginationState{last: -1, total: -1}
	for page := 1; *pages > 0; page++ {
		*pages--
		query.Set("page", strconv.Itoa(page))
		var rows boundedPage[T]
		meta, err := c.get(ctx, remaining, path, query, &rows)
		if err != nil {
			return nil, err
		}
		if rows == nil || len(rows) > pageSize {
			return nil, fail(Response)
		}
		done, err := state.accept(len(rows), meta)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			key := id(row)
			if key <= 0 || seen[key] {
				return nil, fail(Response)
			}
			seen[key] = true
		}
		out = append(out, rows...)
		if done {
			return out, nil
		}
	}
	return nil, fail(Limit)
}

// scrub masks even a token maliciously reflected into a successful DTO. JSON
// was already decoded, so escaped spellings cannot bypass the replacement.
func (c *Client) scrub(out any) {
	clean := func(s string) string { return strings.ReplaceAll(s, string(*c.token), "<redacted>") }
	switch v := out.(type) {
	case *Project:
		v.Name = clean(v.Name)
		v.PathWithNamespace = clean(v.PathWithNamespace)
		v.DefaultBranch = clean(v.DefaultBranch)
		v.WebURL = clean(v.WebURL)
	case *Issue:
		v.Title = clean(v.Title)
		v.Description = clean(v.Description)
		v.State = clean(v.State)
		v.WebURL = clean(v.WebURL)
	case *MergeRequest:
		v.Title = clean(v.Title)
		v.Description = clean(v.Description)
		v.State = clean(v.State)
		v.WebURL = clean(v.WebURL)
		v.SHA = clean(v.SHA)
		v.SourceBranch = clean(v.SourceBranch)
		v.TargetBranch = clean(v.TargetBranch)
	case *Pipeline:
		v.SHA = clean(v.SHA)
		v.Ref = clean(v.Ref)
		v.Status = clean(v.Status)
		v.WebURL = clean(v.WebURL)
		v.YAMLErrors = clean(v.YAMLErrors)
		v.FailureReason = clean(v.FailureReason)
		v.DetailedStatus.Text = clean(v.DetailedStatus.Text)
	case *Job:
		v.Name = clean(v.Name)
		v.Stage = clean(v.Stage)
		v.Status = clean(v.Status)
		v.WebURL = clean(v.WebURL)
		c.scrub(&v.Pipeline)
	}
}
