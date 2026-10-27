package auth

import (
	"errors"
	"strings"
	"testing"
)

// fastArgon2 — параметры для тестов: хэш считается десятки раз, 64 MiB
// умолчаний сделали бы пакет неповоротливым. Формат строки тот же.
func fastArgon2() Argon2Params {
	return Argon2Params{MemoryKiB: 8, Iterations: 1, Parallelism: 1,
		SaltLen: 8, KeyLen: 16}
}

func TestHashPasswordFormat(t *testing.T) {
	h, err := HashPassword("secret", fastArgon2())
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=8,t=1,p=1$") {
		t.Fatalf("unexpected prefix: %q", h)
	}
	parts := strings.Split(h, "$")
	if len(parts) != 6 {
		t.Fatalf("want 6 fields, got %d", len(parts))
	}
}

func TestHashPasswordUniqueSalts(t *testing.T) {
	a, _ := HashPassword("secret", fastArgon2())
	b, _ := HashPassword("secret", fastArgon2())
	if a == b {
		t.Fatal("two hashes of one password must differ (salt)")
	}
}

func TestVerifyPassword(t *testing.T) {
	h, err := HashPassword("secret", fastArgon2())
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if ok, err := VerifyPassword("secret", h); err != nil || !ok {
		t.Fatalf("correct password: ok=%v err=%v", ok, err)
	}
	if ok, err := VerifyPassword("wrong", h); err != nil || ok {
		t.Fatalf("wrong password: ok=%v err=%v", ok, err)
	}
}

func TestVerifyPasswordEmptyPassword(t *testing.T) {
	if _, err := HashPassword("", fastArgon2()); !errors.Is(err, ErrEmptyPassword) {
		t.Fatalf("empty password: err=%v", err)
	}
	h, _ := HashPassword("secret", fastArgon2())
	if _, err := VerifyPassword("", h); !errors.Is(err, ErrEmptyPassword) {
		t.Fatalf("empty verify: err=%v", err)
	}
}

func TestVerifyPasswordRejectsForeignHashes(t *testing.T) {
	cases := map[string]string{
		"empty":        "",
		"not phc":      "plaintext",
		"wrong alg":    "$argon2i$v=19$m=8,t=1,p=1$AAAA$AAAA",
		"wrong ver":    "$argon2id$v=16$m=8,t=1,p=1$AAAA$AAAA",
		"bad params":   "$argon2id$v=19$m=8,x=1,p=1$AAAA$AAAA",
		"zero param":   "$argon2id$v=19$m=0,t=1,p=1$AAAA$AAAA",
		"bad salt b64": "$argon2id$v=19$m=8,t=1,p=1$!!$AAAA",
		"bad hash b64": "$argon2id$v=19$m=8,t=1,p=1$AAAA$!!",
		"extra field":  "$argon2id$v=19$m=8,t=1,p=1$AAAA$AAAA$AAAA",
	}
	for name, h := range cases {
		if _, err := VerifyPassword("secret", h); !errors.Is(err, ErrBadHash) {
			t.Errorf("%s: want ErrBadHash, got %v", name, err)
		}
	}
}

// Использованные в тестах параметры должны оставаться валидными хэшем:
// проверка прочтёт их из строки и примет правильный пароль.
func TestHashPasswordCustomParamsRoundTrip(t *testing.T) {
	h, err := HashPassword("pw", fastArgon2())
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := VerifyPassword("pw", h)
	if err != nil || !ok {
		t.Fatalf("round trip: ok=%v err=%v", ok, err)
	}
}

// Дефолтные параметры и умолчание при нулях дают рабочий хэш.
func TestDefaultArgon2(t *testing.T) {
	if got := DefaultArgon2(); got.MemoryKiB != 64*1024 || got.Iterations != 2 ||
		got.Parallelism != 4 || got.SaltLen != 16 || got.KeyLen != 32 {
		t.Fatalf("unexpected defaults: %+v", got)
	}
	// Нулевые параметры заменяются умолчаниями — один медленный хэш.
	h, err := HashPassword("pw", Argon2Params{})
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if ok, err := VerifyPassword("pw", h); err != nil || !ok {
		t.Fatalf("verify: ok=%v err=%v", ok, err)
	}
}

// BootstrapAdmin принимает хэш от HashPassword.
func TestPasswordHashBoundsBeforeAllocation(t *testing.T) {
	for _, params := range []string{"m=4294967295,t=1,p=1", "m=8,t=4294967295,p=1", "m=8,t=1,p=255", "m=8,t=1,p=1junk"} {
		_, _, _, err := parsePasswordHash("$argon2id$v=19$" + params + "$AAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAA")
		if !errors.Is(err, ErrBadHash) {
			t.Errorf("accepted unsafe parameters %s: %v", params, err)
		}
	}
	for _, field := range []string{strings.Repeat("A", 2048)} {
		_, _, _, err := parsePasswordHash("$argon2id$v=19$m=8,t=1,p=1$" + field + "$" + field)
		if !errors.Is(err, ErrBadHash) {
			t.Errorf("accepted oversized PHC fields: %v", err)
		}
	}
	p := fastArgon2()
	p.SaltLen = -1
	// Negative salt length must return an error, never panic.
	defer func() {
		if v := recover(); v != nil {
			t.Errorf("unsafe config panicked: %v", v)
		}
	}()
	if _, err := HashPassword("pw", p); err == nil {
		t.Error("accepted negative salt length")
	}
}

func TestBootstrapAdminAcceptsHash(t *testing.T) {
	h, err := HashPassword("pw", fastArgon2())
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	st := testStore(t)
	created, err := st.BootstrapAdmin(t.Context(), "root", h)
	if err != nil || !created {
		t.Fatalf("bootstrap: created=%v err=%v", created, err)
	}
	u, err := st.GetUserByUsername(t.Context(), "root")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ok, err := VerifyPassword("pw", u.PasswordHash); err != nil || !ok {
		t.Fatalf("verify bootstrap hash: ok=%v err=%v", ok, err)
	}
	if u.ID == "" || len(u.ID) != 26 {
		t.Fatalf("want ULID id, got %q", u.ID)
	}
}
