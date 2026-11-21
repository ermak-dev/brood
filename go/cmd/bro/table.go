// Таблицы, возраст и вопросы подтверждения: общий вывод команд bro.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"
)

// table — выровненная таблица без рамок. Tone красит строку целиком уже
// после выравнивания, поэтому escape-коды не сдвигают колонки.
type table struct {
	header []string
	rows   [][]string
	tones  []string
}

const (
	toneNone   = ""
	toneBold   = "\x1b[1m"
	toneRed    = "\x1b[31m"
	toneYellow = "\x1b[33m"
	toneDim    = "\x1b[2m"
	toneReset  = "\x1b[0m"
)

func (t *table) add(tone string, cells ...string) {
	t.rows = append(t.rows, cells)
	t.tones = append(t.tones, tone)
}

// write печатает таблицу; цвет — только когда w — терминал.
func (t *table) write(w io.Writer) error {
	var buffer bytes.Buffer
	tab := tabwriter.NewWriter(&buffer, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tab, strings.Join(t.header, "\t"))
	for _, row := range t.rows {
		fmt.Fprintln(tab, strings.Join(row, "\t"))
	}
	if err := tab.Flush(); err != nil {
		return err
	}
	colour := colourful(w)
	tones := append([]string{toneBold}, t.tones...)
	for i, line := range strings.Split(strings.TrimRight(buffer.String(), "\n"), "\n") {
		line = strings.TrimRight(line, " ")
		if colour && tones[i] != toneNone {
			line = tones[i] + line + toneReset
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func colourful(w io.Writer) bool {
	return isTerminal(w) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
}

// isTerminal сообщает, что поток — файл терминала.
func isTerminal(stream any) bool {
	file, ok := stream.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func writeJSON(w io.Writer, value any) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// age — относительный возраст отметки RFC 3339: 40s, 5m, 3h, 2d.
func age(stamp string, now time.Time) string {
	created, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return "-"
	}
	elapsed := now.Sub(created)
	switch {
	case elapsed < 0:
		return "0s"
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds", int(elapsed.Seconds()))
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	case elapsed < 48*time.Hour:
		return fmt.Sprintf("%dh", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%dd", int(elapsed.Hours()/24))
	}
}

// brief сжимает причину до одной короткой фразы для колонки таблицы.
func brief(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if sentence, _, found := strings.Cut(text, ". "); found {
		text = sentence
	}
	if runes := []rune(text); len(runes) > limit {
		text = string(runes[:limit-3]) + "..."
	}
	return text
}

// confirm спрашивает согласие на терминале. Без терминала отвечать некому:
// отказ называет --yes.
func (c *command) confirm(question string, yes bool) error {
	if yes {
		return nil
	}
	if !isTerminal(c.in) {
		return errors.New("confirmation needs a terminal; pass --yes to proceed without asking")
	}
	answer, err := readLine(c.in, question+" [y/N] ", c.diagnostic)
	if err != nil {
		return err
	}
	if answer = strings.ToLower(strings.TrimSpace(answer)); answer != "y" && answer != "yes" {
		return errors.New("cancelled")
	}
	return nil
}

func newFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

// parseArgs разбирает флаги вперемешку с позиционными аргументами; после
// "--" флагов нет. При verbatim > 0 всё после стольких позиционных
// аргументов — текст как есть, даже с дефисом в начале.
func parseArgs(flags *flag.FlagSet, args []string, verbatim int) ([]string, error) {
	var positional []string
	for len(args) > 0 {
		if verbatim > 0 && len(positional) >= verbatim {
			return append(positional, args...), nil
		}
		if err := flags.Parse(args); err != nil {
			text := strings.Replace(err.Error(), "flag provided but not defined: ", "unknown option ", 1)
			return nil, fmt.Errorf("%s; see: bro %s --help", text, flags.Name())
		}
		consumed := len(args) - flags.NArg()
		terminated := consumed > 0 && args[consumed-1] == "--"
		if args = flags.Args(); terminated {
			return append(positional, args...), nil
		}
		if len(args) > 0 {
			positional, args = append(positional, args[0]), args[1:]
		}
	}
	return positional, nil
}
