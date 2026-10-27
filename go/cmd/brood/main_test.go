// Проверки run-цикла: подкоманды, коды выхода, bootstrap-admin на
// подставленном вводе.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersion — подкоманда печатает версию.
func TestVersion(t *testing.T) {
	var out, errOut strings.Builder
	code := run([]string{"version"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("version exit = %d, stderr %s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != version {
		t.Fatalf("version output = %q", out.String())
	}
}

// TestUnknownCommand — незнакомая подкоманда: код 1 и подсказка.
func TestUnknownCommand(t *testing.T) {
	var errOut strings.Builder
	code := run([]string{"bogus"}, strings.NewReader(""), &strings.Builder{}, &errOut)
	if code != 1 {
		t.Fatalf("unknown command exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

// TestNoCommand — без аргументов: код 1.
func TestNoCommand(t *testing.T) {
	var out strings.Builder
	if code := run(nil, strings.NewReader(""), &out, &out); code != 1 {
		t.Fatalf("no command exit = %d", code)
	}
}

// TestBootstrapAdmin — интерактивный завод администратора на подстав
// ленном вводе: пароль идёт в вводе, не в аргументах.
func TestBootstrapAdmin(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "brood.db")
	t.Setenv("BROOD_DB_PATH", dbPath)

	var out, errOut strings.Builder
	code := run([]string{"bootstrap-admin"},
		strings.NewReader("root\nroot-password-1\n"), &out, &errOut)
	if code != 0 {
		t.Fatalf("bootstrap-admin exit = %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "created") {
		t.Fatalf("bootstrap output = %q", out.String())
	}

	// Повтор того же имени — nothing changed, код 0.
	out.Reset()
	code = run([]string{"bootstrap-admin"},
		strings.NewReader("root\nroot-password-1\n"), &out, &errOut)
	if code != 0 {
		t.Fatalf("second bootstrap-admin exit = %d, stderr %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "nothing changed") {
		t.Fatalf("second bootstrap output = %q", out.String())
	}

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database not created: %v", err)
	}
}

// TestBootstrapAdminEmptyUsername — пустое имя: отказ без записи.
func TestBootstrapAdminEmptyUsername(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BROOD_DB_PATH", filepath.Join(dir, "brood.db"))
	var out, errOut strings.Builder
	code := run([]string{"bootstrap-admin"},
		strings.NewReader("\nwhatever-password-1\n"), &out, &errOut)
	if code != 1 {
		t.Fatalf("empty username exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "username") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}
