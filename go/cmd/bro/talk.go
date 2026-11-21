// Работа с агентом: сообщение, хвост его вывода.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"brood.dev/go/brood/clientapi"
	catalog "brood.dev/go/brood/command"
)

// Хвост — снимок экрана агента: чаще он не меняется.
const tailPollInterval = 2 * time.Second

const (
	// tailMaxLines — предел lines инструмента tail.
	tailMaxLines = 10000
	// tailBlankMargin — запас на пустой низ экрана.
	tailBlankMargin = 100
)

func (c *command) send(ctx context.Context, args []string) error {
	flags := newFlags("send")
	wait := flags.Int("wait", 0, "")
	words, err := parseArgs(flags, args, 2)
	if err != nil {
		return err
	}
	if len(words) < 2 {
		return errors.New("expected an agent name and a message; see: bro send --help")
	}
	name, message := words[0], strings.Join(words[1:], " ")
	if len(words) == 2 && words[1] == "-" {
		data, err := io.ReadAll(io.LimitReader(c.in, catalog.MaxArgumentsBytes))
		if err != nil {
			return errors.New("could not read the message from stdin")
		}
		message = strings.TrimRight(string(data), "\r\n")
	}
	if strings.TrimSpace(message) == "" {
		return errors.New("the message is empty")
	}
	result, err := c.invoke(ctx, "send", map[string]any{"to": name, "message": message, "wait_seconds": *wait})
	verdict, delivered, err := delivery(name, result, err)
	if err != nil {
		return named(name, err)
	}
	fmt.Fprintln(c.out, verdict)
	if !delivered {
		return errSilent
	}
	return nil
}

// delivery переводит исход send в ответ человеку. Ошибка остаётся ошибкой,
// когда сервер отказал по другой причине.
func delivery(name string, result catalog.Result, err error) (verdict string, delivered bool, _ error) {
	unconfirmed := "sent to " + name + "; delivery not confirmed, check with: bro tail " + name
	var refusal *clientapi.CommandError
	switch {
	case err == nil && result.Outcome == "completed":
		return "delivered to " + name, true, nil
	case err == nil:
		return unconfirmed, false, nil
	case errors.As(err, &refusal) && refusal.Unconfirmed:
		return unconfirmed, false, nil
	case errors.As(err, &refusal) && strings.Contains(refusal.Reason, "not connected"):
		return name + " is not connected; not sent", false, nil
	}
	return "", false, err
}

func (c *command) tail(ctx context.Context, args []string) error {
	flags := newFlags("tail")
	lines := flags.Int("n", 40, "")
	follow := flags.Bool("f", false, "")
	grep := flags.String("grep", "", "")
	names, err := parseArgs(flags, args, 0)
	if err != nil {
		return err
	}
	if len(names) != 1 {
		return errors.New("expected one agent name; see: bro tail --help")
	}
	if *lines < 1 || *lines > tailMaxLines {
		return fmt.Errorf("-n must be between 1 and %d", tailMaxLines)
	}
	// Низ экрана агента обычно пуст, и сервер считает пустые строки тоже:
	// просим с запасом и оставляем последние строки с текстом.
	arguments := map[string]any{"name": names[0], "lines": min(*lines+tailBlankMargin, tailMaxLines)}
	if *grep != "" {
		arguments["grep"] = *grep
	}
	read := func() ([]string, error) {
		result, err := c.invoke(ctx, "tail", arguments)
		if err != nil {
			return nil, named(names[0], err)
		}
		text := screenLines(result.Text)
		return text[max(0, len(text)-*lines):], nil
	}
	shown, err := read()
	if err != nil {
		return err
	}
	c.printLines(shown)
	if !*follow {
		return nil
	}
	return c.follow(ctx, shown, read)
}

// screenLines — строки хвоста без пустого низа экрана.
func screenLines(text string) []string {
	text = strings.TrimRight(strings.ReplaceAll(text, "\r\n", "\n"), "\n \t")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func (c *command) printLines(lines []string) {
	for _, line := range lines {
		fmt.Fprintln(c.out, line)
	}
}

// follow опрашивает хвост и печатает только новые строки до прерывания.
func (c *command) follow(ctx context.Context, shown []string, read func() ([]string, error)) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	ticker := time.NewTicker(tailPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		current, err := read()
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		c.printLines(fresh(shown, current))
		shown = current
	}
}

// fresh — строки current после самого длинного их общего куска с концом
// shown: хвост сдвигается, прежние строки уходят вверх.
func fresh(shown, current []string) []string {
	for start := 0; start < len(shown); start++ {
		overlap := shown[start:]
		if len(overlap) <= len(current) && equal(overlap, current[:len(overlap)]) {
			return current[len(overlap):]
		}
	}
	return current
}

func equal(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return len(a) == len(b)
}
