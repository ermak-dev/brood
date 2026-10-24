// Общее у командлетов: отказ строкой с причиной, git в рабочей копии,
// чтение тела задачи.
package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Fail — отказ строкой с причиной, не трассировкой: её читает модель.
func Fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "brood: "+format+"\n", args...)
	os.Exit(1)
}

// Usage — справка и выход 2. Докстринг командлета — единственный текст
// справки: второй экземпляр разошёлся бы с первым.
func Usage(doc string) {
	fmt.Fprint(os.Stderr, doc)
	os.Exit(2)
}

// Git — git в рабочей копии. Отказ строкой: трассировка здесь скрывает
// единственное, что нужно знать.
func Git(args ...string) string {
	cmd := exec.Command("git", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		Fail("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out))
}

// DefaultBranch — интеграционная ветка как origin/<имя>: origin/HEAD
// выставлен не в каждом клоне — откат на main, потому что отказ здесь
// означал бы «не могу завести ветку» там, где ветку завести можно.
func DefaultBranch() string {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "origin/HEAD").Output()
	if err != nil {
		return "origin/main"
	}
	ref := strings.TrimSpace(string(out))
	if ref == "" || ref == "origin/HEAD" {
		return "origin/main"
	}
	return ref
}

// Escapes — управляющие последовательности и символы строки чужого вывода:
// в таблице они красили бы его и двигали курсор, а ширина считается по
// видимому.
var escapesRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[@-_]|[\x00-\x08\x0b-\x1f\x7f]`)

// Plain — текст без цветов и управляющих символов терминала: вывод
// командлета, который читает модель, а не терминал.
func Plain(text string) string {
	return escapesRe.ReplaceAllString(text, "")
}

// ReadBody — тело задачи: --body-file, аргумент или stdin. Через файл и
// stdin — ради бэктиков и переносов: шелл их съедает, а тело задачи без
// них не написать.
func ReadBody(text, path string) string {
	if path == "-" {
		raw, _ := io.ReadAll(os.Stdin)
		return strings.TrimSpace(string(raw))
	}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			Fail("--body-file %s: %v", path, err)
		}
		return strings.TrimSpace(string(raw))
	}
	if text != "" && text != "-" {
		return strings.TrimSpace(text)
	}
	raw, _ := io.ReadAll(os.Stdin)
	return strings.TrimSpace(string(raw))
}
