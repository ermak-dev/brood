package server

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"brood.dev/go/brood/gitorigin"
)

const (
	gitRemotesSetting  = "BROOD_GIT_REMOTES"
	gitRemotesMaxCount = 256
	gitUsernameMax     = 256
	gitTokenMax        = 4096
)

// gitCredential — учётные данные установки для одного remote. Только сервер
// подставляет их в upstream-запрос шлюза; в JSON, логи, ошибки и окружение
// гостя они не попадают, форматирование их скрывает.
type gitCredential struct {
	username, token string
}

func (gitCredential) String() string               { return "server.gitCredential{redacted}" }
func (c gitCredential) GoString() string           { return c.String() }
func (gitCredential) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// Format закрывает любой verb: String и GoString fmt зовёт только для %v,
// %s, %q, %x, %X и %#v, остальные печатали поля (%d давал
// {%!d(string=token)}). Тот же Format у каждого держателя Git credential.
// Остаётся %p не-указателя: его fmt печатает рефлексией мимо методов, такой
// вызов отвергает go vet.
func (c gitCredential) Format(f fmt.State, _ rune) { redactFormat(f, c) }

// redactFormat печатает скрытое представление держателя credential.
func redactFormat(f fmt.State, s fmt.Stringer) { _, _ = io.WriteString(f, s.String()) }

// configured сообщает, есть ли у remote учётные данные (Basic auth).
func (c gitCredential) configured() bool { return c.username != "" && c.token != "" }

// gitRemotes — remotes установки по каноническому HTTPS URL репозитория.
type gitRemotes map[string]gitCredential

func (gitRemotes) String() string               { return "server.gitRemotes{redacted}" }
func (r gitRemotes) GoString() string           { return r.String() }
func (gitRemotes) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (r gitRemotes) Format(f fmt.State, _ rune) { redactFormat(f, r) }

// origins — настроенные remotes в стабильном порядке.
func (r gitRemotes) origins() []string {
	out := make([]string, 0, len(r))
	for origin := range r {
		out = append(out, origin)
	}
	sort.Strings(out)
	return out
}

// loadGitRemotes разбирает BROOD_GIT_REMOTES. Ключ обязан быть уже
// каноническим: иначе два написания одного репозитория дали бы два remote.
// Отказ называет настройку и правило, но не значение.
func loadGitRemotes(raw string) (gitRemotes, error) {
	remotes := gitRemotes{}
	if raw == "" {
		return remotes, nil
	}
	fields, err := proxmoxObject(raw)
	if err != nil {
		return nil, gitRemotesError("must be a JSON object with unique keys")
	}
	if len(fields) > gitRemotesMaxCount {
		return nil, gitRemotesError("must contain at most 256 remotes")
	}
	for origin, value := range fields {
		if canonical, err := gitorigin.Canonical(origin); err != nil || canonical != origin {
			return nil, gitRemotesError("keys must be canonical HTTPS repository URLs")
		}
		credential, err := parseGitCredential(value)
		if err != nil {
			return nil, err
		}
		remotes[origin] = credential
	}
	return remotes, nil
}

func parseGitCredential(raw json.RawMessage) (gitCredential, error) {
	var credential gitCredential
	fields, err := proxmoxObject(string(raw))
	if err != nil {
		return credential, gitRemotesError("values must be objects with unique fields")
	}
	if len(fields) == 0 {
		return credential, nil
	}
	known := map[string]*string{"username": &credential.username, "token": &credential.token}
	for field := range fields {
		if _, ok := known[field]; !ok {
			return credential, gitRemotesError("values accept only username and token")
		}
	}
	for field, target := range known {
		value, ok := fields[field]
		if !ok || json.Unmarshal(value, target) != nil || strings.TrimSpace(string(value)) == "null" {
			return credential, gitRemotesError("username and token must be set together as strings")
		}
	}
	if !proxmoxHeaderValue(credential.username) || strings.Contains(credential.username, ":") || len(credential.username) > gitUsernameMax {
		return credential, gitRemotesError("username must be visible ASCII without colons, at most 256 bytes")
	}
	if !proxmoxHeaderValue(credential.token) || len(credential.token) > gitTokenMax {
		return credential, gitRemotesError("token must be visible ASCII, at most 4096 bytes")
	}
	return credential, nil
}

func gitRemotesError(detail string) error {
	return &ConfigError{Setting: gitRemotesSetting, Detail: detail}
}
