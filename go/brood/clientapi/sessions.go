package clientapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sessionFileName — имя файла сессии для origin: base64-url без двоеточий
// и слэшей, безопасно для файловых систем и однозначно для каждого origin.
func sessionFileName(origin string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(origin)) + ".json"
}

// storedFile — формат файла сессии на диске.
type storedFile struct {
	Origin  string `json:"origin"`
	Session string `json:"session"`
	CSRF    string `json:"csrf"`
	CAPEM   string `json:"ca_pem,omitempty"`
}

// SessionStore хранит сессии по server origin в пользовательском каталоге
// состояния. Файлы 0600, каталог 0700, запись атомарная (temp+rename).
// Пароль не сохраняется никогда: ему здесь просто нет места.
type SessionStore struct {
	dir string
}

// NewSessionStore создаёт хранилище в заданном каталоге.
func NewSessionStore(dir string) (*SessionStore, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("session store directory must be an absolute path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	// Каталог принадлежит нам: фиксируем 0700 поверх umask и старых прав.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("chmod session directory: %w", err)
	}
	return &SessionStore{dir: dir}, nil
}

// Load возвращает сохранённую сессию origin; false, если её нет.
func (s *SessionStore) Load(origin string) (StoredSession, bool, error) {
	raw, err := os.ReadFile(s.path(origin))
	if errors.Is(err, os.ErrNotExist) {
		return StoredSession{}, false, nil
	}
	if err != nil {
		return StoredSession{}, false, fmt.Errorf("read session file: %w", err)
	}
	var f storedFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return StoredSession{}, false, fmt.Errorf("malformed session file: %w", err)
	}
	if f.Origin != origin {
		return StoredSession{}, false, fmt.Errorf("session file belongs to %q, want %q", f.Origin, origin)
	}
	return StoredSession{Session: f.Session, CSRF: f.CSRF, CAPEM: f.CAPEM}, true, nil
}

// Save атомарно записывает сессию origin с правами 0600.
func (s *SessionStore) Save(origin string, sess StoredSession) error {
	raw, err := json.MarshalIndent(storedFile{
		Origin:  origin,
		Session: sess.Session,
		CSRF:    sess.CSRF,
		CAPEM:   sess.CAPEM,
	}, "", "  ")
	if err != nil {
		return err
	}
	return s.writeAtomic(s.path(origin), raw)
}

// writeAtomic пишет через временный файл и rename: файл либо старый,
// либо новый целиком.
func (s *SessionStore) writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(s.dir, ".session-*")
	if err != nil {
		return fmt.Errorf("create temp session file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op после успешного rename
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod temp session file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp session file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp session file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace session file: %w", err)
	}
	return nil
}

// Delete удаляет сохранённую сессию origin. Отсутствие файла — успех.
func (s *SessionStore) Delete(origin string) error {
	if err := os.Remove(s.path(origin)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove session file: %w", err)
	}
	return nil
}

// Current возвращает выбранный сервер или пустую строку, если выбора нет.
func (s *SessionStore) Current() (string, error) {
	raw, err := os.ReadFile(s.currentPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read current server file: %w", err)
	}
	cur := strings.TrimSpace(string(raw))
	if cur == "" {
		return "", nil
	}
	if _, err := NormalizeOrigin(cur); err != nil {
		return "", fmt.Errorf("malformed current server file: %w", err)
	}
	return cur, nil
}

// SetCurrent запоминает выбранный сервер; пустой origin снимает выбор.
func (s *SessionStore) SetCurrent(origin string) error {
	if origin == "" {
		if err := os.Remove(s.currentPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove current server file: %w", err)
		}
		return nil
	}
	return s.writeAtomic(s.currentPath(), []byte(origin+"\n"))
}

func (s *SessionStore) path(origin string) string {
	return filepath.Join(s.dir, sessionFileName(origin))
}
func (s *SessionStore) currentPath() string { return filepath.Join(s.dir, "current") }
