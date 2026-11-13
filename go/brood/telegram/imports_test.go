package telegram

// FEATURE: The Telegram client stands alone (#280).
// SPEC: The package imports only the standard library: no store, no server,
// no new module dependency (docs/GO.md). It imports no logger, so the token
// cannot reach a log from here.

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestImportsOnlyTheStandardLibrary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		seen++
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if strings.Contains(strings.Split(path, "/")[0], ".") {
				t.Errorf("%s imports %s: the package uses only the standard library", name, path)
			}
			if path == "log" || path == "log/slog" {
				t.Errorf("%s imports %s: the package writes no logs", name, path)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no package sources found")
	}
}
