// Package clientapi — клиент веб-API Brood: вход, сессии и минимальные
// пользовательские операции для командлета bro. Библиотека молчит и возвращает
// данные; печать — только в cmd.
package clientapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Отправляемые и принимаемые cookie имена контракта CONTRACT.md.
const (
	cookieSession = "session"
	cookieCSRF    = "csrf"
	headerCSRF    = "X-CSRFToken"
)

// Сентинельные ошибки: CLI сопоставляет их с сообщениями и кодами выхода.
var (
	// ErrInvalidCredentials — сервер отказал в входе (401 на login).
	ErrInvalidCredentials = errors.New("invalid credentials")
	// ErrSessionExpired — сессия отсутствует, истекла или отозвана.
	ErrSessionExpired = errors.New("session expired")
	// ErrRedirect — сервер ответил перенаправлением; клиент его не следует.
	ErrRedirect = errors.New("server redirect refused")
)

// User — краткие данные пользователя из /api/auth/session.
type User struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsAdmin     bool   `json:"is_admin"`
}

// SessionInfo — ответ GET /api/auth/session.
type SessionInfo struct {
	Authenticated bool  `json:"authenticated"`
	User          *User `json:"user"`
}

// Project — строка списка проектов.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// StoredSession — сохраняемые между запусками cookie по server origin.
// Пароль здесь не хранится никогда.
type StoredSession struct {
	Session string `json:"session"`
	CSRF    string `json:"csrf"`
	// CAPEM is additional trust pinned to this origin, never a path into another machine.
	CAPEM string `json:"ca_pem,omitempty"`
}

func (s StoredSession) empty() bool { return s.Session == "" && s.CSRF == "" }

// Empty сообщает, хранится ли в значении какая-либо сессия.
func (s StoredSession) Empty() bool { return s.empty() }

// Client — HTTP-клиент одного server origin. Cookie обрабатываются явно,
// без общего jar: значения живут в структуре и не утекают к другим origin.
type Client struct {
	origin string
	http   *http.Client

	mu     sync.Mutex
	stored StoredSession
}

// Option — настройка клиента.
type Option func(*clientOptions)

type clientOptions struct {
	caPEM []byte
}

// WithCACert доверяет дополнительному CA в PEM вместо ослабления проверки.
func WithCACert(pem []byte) Option {
	return func(o *clientOptions) { o.caPEM = pem }
}

// NormalizeOrigin приводит адрес сервера к виду scheme://host[:port]:
// без схемы подставляется https, путь и фрагмент отбрасываются.
func NormalizeOrigin(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty server address")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return "", errors.New("invalid server address")
	}
	if u.User != nil {
		return "", errors.New("server address must not contain credentials")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("server address must use HTTPS")
	}
	if u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("server address must not contain path, query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	ip := net.ParseIP(host)
	if u.Scheme == "http" && host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", errors.New("server must use HTTPS except for localhost/loopback development")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path, u.RawQuery, u.Fragment = "", "", ""
	return u.String(), nil
}

// New создаёт клиента для server origin. Проверка TLS включена всегда;
// WithCACert добавляет доверенный CA, опции отключения проверки нет.
func New(origin string, opts ...Option) (*Client, error) {
	norm, err := NormalizeOrigin(origin)
	if err != nil {
		return nil, err
	}
	cfg := clientOptions{}
	for _, opt := range opts {
		opt(&cfg)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if len(cfg.caPEM) > 0 {
		pool, err := newCAPool(cfg.caPEM)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{
		origin: norm,
		// Перенаправления не следуются автоматически: секреты и cookie
		// не должны уезжать на другой origin без явного решения.
		http: &http.Client{
			Transport: transport,
			Timeout:   30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Origin возвращает нормализованный origin сервера.
func (c *Client) Origin() string { return c.origin }

// Stored копию текущих сохранённых cookie.
func (c *Client) Stored() StoredSession {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stored
}

// Restore подставляет ранее сохранённые cookie (файл сессии).
func (c *Client) Restore(s StoredSession) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stored = s
}

// Session вызывает GET /api/auth/session: анонимно — bootstrap csrf,
// с сохранённой сессией — статус пользователя.
func (c *Client) Session(ctx context.Context) (*SessionInfo, error) {
	var info SessionInfo
	if err := c.call(ctx, http.MethodGet, "/api/auth/session", nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// Login входит по имени и паролю: bootstrap csrf при необходимости,
// POST /api/auth/login с X-CSRFToken, затем статус для полных данных
// пользователя. Пароль не попадает в ошибки и логи.
func (c *Client) Login(ctx context.Context, username, password string) (*SessionInfo, error) {
	// Отозванная сессия требует нового анонимного CSRF перед повторным входом.
	if _, err := c.Session(ctx); err != nil {
		return nil, err
	}
	if c.Stored().CSRF == "" {
		return nil, errors.New("server did not provide a CSRF cookie")
	}
	body, err := json.Marshal(map[string]string{
		"username": username,
		"password": password,
	})
	if err != nil {
		return nil, err
	}
	if err := c.call(ctx, http.MethodPost, "/api/auth/login", body, nil); err != nil {
		return nil, err
	}
	return c.Session(ctx)
}

// Logout отзывает серверную сессию. При успехе локальные cookie чистятся;
// при ошибке сети или отказа сервера cookie остаются — отзыв не выдан.
func (c *Client) Logout(ctx context.Context) error {
	stored := c.Stored()
	if stored.empty() {
		// Отзывать нечего: считаем успехом, локального состояния нет.
		return nil
	}
	body, err := json.Marshal(map[string]string{})
	if err != nil {
		return err
	}
	if err := c.call(ctx, http.MethodPost, "/api/auth/logout", body, nil); err != nil {
		if errors.Is(err, ErrSessionExpired) {
			// Сессии на сервере уже нет: локальную копию можно снять.
			c.Restore(StoredSession{})
			return nil
		}
		return err
	}
	c.Restore(StoredSession{})
	return nil
}

// Whoami возвращает пользователя текущей сессии.
func (c *Client) Whoami(ctx context.Context) (*User, error) {
	info, err := c.Session(ctx)
	if err != nil {
		return nil, err
	}
	if !info.Authenticated || info.User == nil {
		return nil, ErrSessionExpired
	}
	return info.User, nil
}

// ProjectsList возвращает доступные проекты текущего пользователя.
// Сервер отдаёт плоский массив; на конверт {"projects": [...]} ответ
// считается несовместимым и отвергается.
func (c *Client) ProjectsList(ctx context.Context) ([]Project, error) {
	var projects []Project
	if err := c.call(ctx, http.MethodGet, "/api/projects", nil, &projects); err != nil {
		return nil, err
	}
	return projects, nil
}

// call выполняет запрос к API: подставляет cookie из хранилища, шлёт
// X-CSRFToken на изменяющие запросы, обновляет хранилище из Set-Cookie.
func (c *Client) call(ctx context.Context, method, path string, body []byte, out any) error {
	return c.callWithTimeout(ctx, method, path, body, out, c.http.Timeout)
}

func (c *Client) callWithTimeout(ctx context.Context, method, path string, body []byte, out any, timeout time.Duration) error {
	var rdr io.Reader
	media := ""
	if body != nil {
		rdr = bytes.NewReader(body)
		media = "application/json"
	}
	resp, err := c.request(ctx, method, path, rdr, int64(len(body)), media, timeout)
	if err != nil {
		if method == http.MethodPost && strings.HasPrefix(path, "/api/commands/") {
			return fmt.Errorf("command outcome is unconfirmed; check before retrying: %w", err)
		}
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return fmt.Errorf("%s %s: %w (status %d)",
			method, path, ErrRedirect, resp.StatusCode)
	}
	// Отказ не меняет cookie. Диагностика сервера недоверенная и может
	// содержать пароль или сессию, поэтому тело ошибки не включаем в отказ.
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		c.absorbCookies(resp)
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusAccepted) && out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			if method == http.MethodPost && strings.HasPrefix(path, "/api/commands/") {
				return fmt.Errorf("command outcome is unconfirmed; check before retrying: malformed response from %s: %w", c.origin, err)
			}
			return fmt.Errorf("malformed response from %s: %w", c.origin, err)
		}
		return nil
	}
	var statusErr error
	switch {
	case resp.StatusCode == http.StatusUnauthorized && method == "POST" && strings.HasSuffix(path, "/login"):
		statusErr = fmt.Errorf("%w", ErrInvalidCredentials)
	case resp.StatusCode == http.StatusUnauthorized:
		statusErr = fmt.Errorf("%w", ErrSessionExpired)
	default:
		statusErr = fmt.Errorf("%s %s: server error (status %d): %s", method, path, resp.StatusCode, http.StatusText(resp.StatusCode))
		if method == http.MethodPut && path == "/api/projects/sync" && resp.StatusCode == http.StatusBadRequest {
			if detail := publicProjectSyncError(resp.Body); detail != "" {
				statusErr = fmt.Errorf("%w: %s", statusErr, detail)
			}
		} else if method == http.MethodPost && strings.HasPrefix(path, "/api/commands/") {
			if reason := publicCommandRefusal(resp.Body); reason != "" {
				statusErr = fmt.Errorf("%w: %s", statusErr, reason)
			}
		}
	}
	if method == http.MethodPost && strings.HasPrefix(path, "/api/commands/") {
		outcome := resp.Header.Get("X-Brood-Command-Outcome")
		if outcome == "unconfirmed" || outcome == "unknown" {
			return fmt.Errorf("command outcome is unconfirmed; check before retrying: %w", statusErr)
		}
	}
	return statusErr
}

// absorbCookies переносит Set-Cookie в хранилище: пустое значение или
// MaxAge<0 означают удаление cookie.
func (c *Client) absorbCookies(resp *http.Response) {
	var updated *StoredSession
	apply := func() *StoredSession {
		if updated == nil {
			s := c.Stored()
			updated = &s
		}
		return updated
	}
	for _, ck := range resp.Cookies() {
		deleted := ck.Value == "" || ck.MaxAge < 0
		switch ck.Name {
		case cookieSession:
			if deleted {
				apply().Session = ""
			} else if ck.Value != "" {
				apply().Session = ck.Value
			}
		case cookieCSRF:
			if deleted {
				apply().CSRF = ""
			} else if ck.Value != "" {
				apply().CSRF = ck.Value
			}
		}
	}
	if updated != nil {
		c.Restore(*updated)
	}
}
