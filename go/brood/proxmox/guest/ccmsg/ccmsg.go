package ccmsg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CHANNEL.md mop: приёмник разбирает конверт регуляркой и сверяет обратную
// сборку байт в байт; порядок атрибутов фиксирован: from, from-name, from-mode.
const envelopeOpen = "<cross-session-message"

// EnvelopeBuilder собирает конверт заявки о режиме прав. Атрибуты необязательны,
// но порядок фиксирован приёмником Claude Code: переставленный конверт не
// распознаётся и сообщение уезжает в held-очередь.
type EnvelopeBuilder struct {
	From     string // uds-адрес отправителя; спецсимволы percent-кодируются
	FromName string // имя для ответа; без `"<>` и переводов строк
	FromMode string // "bypass" | "prompting"
}

func percentEncode(s string) string {
	// `/`, `:`, `.`, `-`, `_` остаются как есть — как в mop.
	const safe = "/:.-_"
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune(safe, r) {
			b.WriteRune(r)
			continue
		}
		for _, b1 := range []byte(string(r)) {
			fmt.Fprintf(&b, "%%%02X", b1)
		}
	}
	return b.String()
}

var fromNameForbidden = regexp.MustCompile(`["<>\r\n]`)

// Build возвращает конверт с телом. Отказ — строкой с причиной (CLAUDE.md).
func (e EnvelopeBuilder) Build(body string) (string, error) {
	if e.FromMode != "" && e.FromMode != "bypass" && e.FromMode != "prompting" {
		return "", errors.New("from-mode must be bypass or prompting")
	}
	if fromNameForbidden.MatchString(e.FromName) {
		return "", errors.New("from-name must not contain quotes, angle brackets or newlines")
	}
	if body == "" {
		return "", errors.New("message body must not be empty")
	}
	// Тело с закрывающим тегом сломает обратную сборку приёмника и молча
	// уедет в held: отказываем при сборке, а не после доставки.
	if strings.Contains(body, "</cross-session-message>") {
		return "", errors.New("message body must not contain the envelope closing tag")
	}
	var attrs strings.Builder
	if e.From != "" {
		attrs.WriteString(fmt.Sprintf(" from=%q", percentEncode(e.From)))
	}
	if e.FromName != "" {
		attrs.WriteString(fmt.Sprintf(" from-name=%q", e.FromName))
	}
	if e.FromMode != "" {
		attrs.WriteString(fmt.Sprintf(" from-mode=%q", e.FromMode))
	}
	return fmt.Sprintf("%s%s>\n%s\n</cross-session-message>", envelopeOpen, attrs.String(), body), nil
}

// Frame — один NDJSON-кадр канала. Идентификатор задаёт отправитель.
type Frame map[string]any

// UserFrame собирает кадр type:user с конвертом в content.
func UserFrame(content, priority, from, msgID string) (Frame, error) {
	if content == "" {
		return nil, errors.New("user frame content must not be empty")
	}
	switch priority {
	case "now", "next", "later", "":
	default:
		return nil, fmt.Errorf("priority must be one of now, next, later")
	}
	if priority == "" {
		priority = "next"
	}
	f := Frame{"msgV": 1, "type": "user",
		"message":  map[string]any{"role": "user", "content": content},
		"priority": priority}
	if msgID != "" {
		f["msg_id"] = msgID
	}
	if from != "" {
		f["from"] = from
	}
	return f, nil
}

// ControlFrame собирает кадр type:control (например notify_when_idle).
func ControlFrame(action, from, fromMode, msgID string) (Frame, error) {
	if action == "" {
		return nil, errors.New("control frame action must not be empty")
	}
	f := Frame{"msgV": 1, "type": "control", "action": action}
	if msgID != "" {
		f["msg_id"] = msgID
	}
	if from != "" {
		f["from"] = from
	}
	if fromMode != "" {
		f["from_mode"] = fromMode
	}
	return f, nil
}

// AuthFrame — необязательный первый кадр соединения; без него подписку
// notify_when_idle сессия молча уронит.
func AuthFrame(token string) Frame {
	return Frame{"type": "auth", "token": token}
}

// EncodeNDJSON собирает кадры в строки протокола, по объекту на строку.
func EncodeNDJSON(frames []Frame) (string, error) {
	var b strings.Builder
	for _, f := range frames {
		line, err := json.Marshal(f)
		if err != nil {
			return "", fmt.Errorf("frame encoding failed: %w", err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// DecodeNDJSON разбирает поток квитанций: по объекту на строку, мусор пропускается.
func DecodeNDJSON(data string) []Frame {
	var out []Frame
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var f Frame
		if json.Unmarshal([]byte(line), &f) == nil {
			out = append(out, f)
		}
	}
	return out
}

// Receipt выбирает из кадров квитанцию про origMsgID: peer_message_status
// (доставка) или peer_idle_notice (подписка на простой).
func Receipt(frames []Frame, action, origMsgID string) (Frame, error) {
	for _, f := range frames {
		if f["action"] == action && f["orig_msg_id"] == origMsgID {
			return f, nil
		}
	}
	return nil, fmt.Errorf("no %s receipt for %s", action, origMsgID)
}

// PeerToken ищет ключ инбокса сессии: <sessions>/<pid>.<sha256(realpath)>.key,
// поле peerToken. Файл переживает грязную смерть процесса — ключ не доказывает
// живости, а без него subscription молча уронят.
func PeerToken(sockPath, sessionsDir string) (string, error) {
	abs, err := filepath.Abs(sockPath)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// Симлинк-адрес приёмник всё равно отвергает; но ключ лежит по
		// каноническому пути — realpath недоступен лишь для мёртвых ссылок.
		real = abs
	}
	sum := sha256.Sum256([]byte(real))
	suffix := fmt.Sprintf(".%s.key", hex.EncodeToString(sum[:]))
	prefix := strings.TrimSuffix(filepath.Base(real), ".sock") + "."
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return "", fmt.Errorf("session directory unavailable: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		data, err := readSessionFile(filepath.Join(sessionsDir, name))
		if err != nil {
			continue
		}
		var key struct {
			PeerToken string `json:"peerToken"`
		}
		if json.Unmarshal(data, &key) == nil && key.PeerToken != "" {
			return key.PeerToken, nil
		}
	}
	return "", errors.New("peer token not found for socket")
}

func readSessionFile(path string) ([]byte, error) {
	const limit = 128 * 1024
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("invalid session file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if len(data) > limit {
		return nil, errors.New("session file exceeds size limit")
	}
	return data, err
}

// Session — запись файла ~/.claude/sessions/<pid>.json, сколько нужно каналу.
type Session struct {
	PID                 int
	Name                string
	SocketPath          string
	Status              string
	StatusUpdatedAtMsec float64
	CWD                 string
}

// FindSessions читает все файлы сессий с адресом инбокса; мёртвые тоже: живость
// решает сокет, а не файл.
func FindSessions(sessionsDir string) ([]Session, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return nil, fmt.Errorf("session directory unavailable: %w", err)
	}
	var out []Session
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := readSessionFile(filepath.Join(sessionsDir, entry.Name()))
		if err != nil {
			continue
		}
		var s struct {
			PID             int     `json:"pid"`
			Name            string  `json:"name"`
			Socket          string  `json:"messagingSocketPath"`
			Status          string  `json:"status"`
			StatusUpdatedAt float64 `json:"statusUpdatedAt"`
			CWD             string  `json:"cwd"`
		}
		if json.Unmarshal(data, &s) != nil || s.Socket == "" {
			continue
		}
		out = append(out, Session{PID: s.PID, Name: s.Name, SocketPath: s.Socket,
			Status: s.Status, StatusUpdatedAtMsec: s.StatusUpdatedAt, CWD: s.CWD})
	}
	// Свежие первыми: на каталог накапливается по файлу на смерть каждой
	// сессии, самая свежая запись бывает трупом.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StatusUpdatedAtMsec > out[j-1].StatusUpdatedAtMsec; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// SessionForCWD выбирает сессию каталога; все живые сокеты проверяются по
// порядку свежести. Ошибки называют и «нет», и «несколько».
func SessionForCWD(sessionsDir, cwd string, alive func(string) bool) (Session, error) {
	sessions, err := FindSessions(sessionsDir)
	if err != nil {
		return Session{}, err
	}
	var mine []Session
	target, terr := filepath.EvalSymlinks(cwd)
	if terr != nil {
		target = cwd
	}
	for _, s := range sessions {
		swd, err := filepath.EvalSymlinks(s.CWD)
		if err != nil {
			swd = s.CWD
		}
		if swd == target {
			mine = append(mine, s)
		}
	}
	var live []Session
	for _, s := range mine {
		if alive(s.SocketPath) {
			live = append(live, s)
		}
	}
	if len(live) > 1 {
		// Имена сессий не уникальны: молча взять первую — написать не тому.
		names := make([]string, 0, len(live))
		for _, s := range live {
			names = append(names, fmt.Sprintf("%s[%d]", s.Name, s.PID))
		}
		return Session{}, fmt.Errorf("'%s' matches several live sessions: %s", cwd, strings.Join(names, ", "))
	}
	if len(live) == 1 {
		return live[0], nil
	}
	if len(mine) == 0 {
		return Session{}, fmt.Errorf("session not found for %s", cwd)
	}
	return Session{}, errors.New("sessions matched but none are listening")
}
