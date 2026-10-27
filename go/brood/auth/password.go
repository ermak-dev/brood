// Пароли: Argon2id в кодировке PHC. Параметры и соль живут в самой
// строке хэша, поэтому смена умолчаний не инвалидирует старые хэши.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
)

// Argon2Params — параметры хэширования пароля. Нули означают умолчание
// (DefaultArgon2) и подставляются в New.
type Argon2Params struct {
	// MemoryKiB — память на один хэш, KiB.
	MemoryKiB uint32
	// Iterations — проходов (t).
	Iterations uint32
	// Parallelism — потоков (p).
	Parallelism uint8
	// SaltLen — длина соли в байтах.
	SaltLen int
	// KeyLen — длина ключа в байтах.
	KeyLen uint32
}

// defArgon2 — умолчание: 64 MiB, t=2, p=4, соль 16 и ключ 32 байта.
// Ориентир OWASP для Argon2id: m=46..71 MiB при t=1..3, p=4.
var defArgon2 = Argon2Params{MemoryKiB: 64 * 1024, Iterations: 2, Parallelism: 4, SaltLen: 16, KeyLen: 32}

// DefaultArgon2 возвращает параметры Argon2id по умолчанию.
func DefaultArgon2() Argon2Params { return defArgon2 }

// Ошибки пароля: значения, не тексты в исключениях.
var (
	// ErrEmptyPassword — пустой пароль не принимается нигде.
	ErrEmptyPassword = errors.New("empty password")
	// ErrBadHash — строка хэша не в нашем формате PHC.
	ErrBadHash = errors.New("malformed password hash")
)

// phcB64 — PHC-кодировка base64 без padding (внутри $-полей).
var phcB64 = base64.RawStdEncoding

// HashPassword хэширует пароль Argon2id и кодирует PHC-строкой
// $argon2id$v=19$m=<KiB>,t=<it>,p=<par>$<salt>$<hash>.
func HashPassword(password string, p Argon2Params) (string, error) {
	if password == "" {
		return "", ErrEmptyPassword
	}
	if !boundedArgon2(p) {
		return "", ErrBadHash
	}
	if p.MemoryKiB == 0 || p.Iterations == 0 || p.Parallelism == 0 ||
		p.SaltLen == 0 || p.KeyLen == 0 {
		p = defArgon2
	}
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB,
		p.Parallelism, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.MemoryKiB, p.Iterations, p.Parallelism,
		phcB64.EncodeToString(salt), phcB64.EncodeToString(key)), nil
}

// VerifyPassword сверяет пароль с PHC-строкой: параметры берутся из
// самой строки, сравнение ключей — постоянное по времени. Отказ
// ErrBadHash означает чужой формат хэша, false — неверный пароль.
func VerifyPassword(password, encoded string) (bool, error) {
	if password == "" {
		return false, ErrEmptyPassword
	}
	p, salt, want, err := parsePasswordHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Iterations, p.MemoryKiB,
		p.Parallelism, p.KeyLen)
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, nil
	}
	return true, nil
}

// Ограничения применяются до выделения памяти как к конфигурации,
// так и к хэшу из базы. Нули конфигурации отдельно заменяются умолчаниями.
func boundedArgon2(p Argon2Params) bool {
	return p.MemoryKiB <= 256*1024 && p.Iterations <= 10 && p.Parallelism <= 16 &&
		p.SaltLen >= 0 && p.SaltLen <= 64 && p.KeyLen <= 64
}

// parsePasswordHash разбирает PHC-строку нашего формата: только argon2id
// версии 19, все пять полей обязательны.
func parsePasswordHash(encoded string) (p Argon2Params, salt, key []byte, err error) {
	if len(encoded) > 512 {
		return p, nil, nil, ErrBadHash
	}
	parts := strings.Split(encoded, "$")
	// Split даёт пустой первый элемент перед ведущим "$".
	if len(parts) != 6 || parts[0] != "" {
		return p, nil, nil, ErrBadHash
	}
	if parts[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("%w: algorithm %q", ErrBadHash, parts[1])
	}
	if parts[2] != "v=19" {
		return p, nil, nil, fmt.Errorf("%w: version %q", ErrBadHash, parts[2])
	}
	var m, t uint32
	var par uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &par); err != nil {
		return p, nil, nil, fmt.Errorf("%w: params %q", ErrBadHash, parts[3])
	}
	p = Argon2Params{MemoryKiB: m, Iterations: t, Parallelism: par,
		SaltLen: phcB64.DecodedLen(len(parts[4])), KeyLen: uint32(phcB64.DecodedLen(len(parts[5])))}
	if !boundedArgon2(p) || parts[3] != fmt.Sprintf("m=%d,t=%d,p=%d", m, t, par) {
		return p, nil, nil, ErrBadHash
	}
	salt, err = phcB64.DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, fmt.Errorf("%w: salt: %v", ErrBadHash, err)
	}
	key, err = phcB64.DecodeString(parts[5])
	if err != nil {
		return p, nil, nil, fmt.Errorf("%w: hash: %v", ErrBadHash, err)
	}
	if m == 0 || t == 0 || par == 0 || len(salt) == 0 || len(key) == 0 {
		return p, nil, nil, fmt.Errorf("%w: zero parameter", ErrBadHash)
	}
	return Argon2Params{MemoryKiB: m, Iterations: t, Parallelism: par,
		SaltLen: len(salt), KeyLen: uint32(len(key))}, salt, key, nil
}

// dummyOnce лениво считает хэш-приманку теми же умолчаниями, что и
// реальный вход: проверка пароля несуществующего пользователя стоит
// столько же времени и не выдаёт существование username таймингом.
var dummyOnce struct {
	sync.Once
	hash string
}

// dummyVerify гоняет проверку пароля по хэшу-приманке. Ошибка игнорируется:
// цель — потратить время, а не проверить.
func dummyVerify(password string) {
	dummyOnce.Do(func() {
		h, err := HashPassword("timing equalizer", defArgon2)
		if err != nil {
			panic("auth: dummy hash: " + err.Error())
		}
		dummyOnce.hash = h
	})
	_, _ = VerifyPassword(password, dummyOnce.hash)
}
