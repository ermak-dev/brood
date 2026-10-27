package acceptance

import (
	"strings"
	"testing"
)

func TestCompilerUsesThePreparedModuleCache(t *testing.T) {
	t.Setenv("GOMODCACHE", t.TempDir())
	t.Setenv("GOFLAGS", "-modcacherw")
	env := strings.Join(compilerEnv(), "\n")
	if !strings.Contains(env, "GOMODCACHE=") || !strings.Contains(env, "GOFLAGS=-modcacherw") {
		t.Fatal("nested offline build discarded prepared module cache or cleanup flags")
	}
}
