// bro attach: консоль агента в терминале человека.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"brood.dev/go/brood/clientapi"
)

const (
	// detachKey — Ctrl-]: агенту не уходит, остальное уходит байт в байт.
	detachKey = 0x1d
	interrupt = 0x03
	// Снимок экрана — три вызова гостя: чаще опрашивать незачем.
	viewInterval = 2 * time.Second
	// Столько снимков подряд может не удаться, прежде чем просмотр сдаётся.
	viewFailures = 5
	// Что приложение агента могло включить в терминале человека: второй
	// экран, мышь, отчёты о фокусе, вставку в скобках, вид курсора, цвет.
	terminalReset = "\x1b[?1049l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?1004l\x1b[?2004l\x1b[0 q\x1b[?25h\x1b[0m"
)

func (c *command) attach(ctx context.Context, args []string) error {
	flags := newFlags("attach")
	view := flags.Bool("view", false, "")
	names, err := parseArgs(flags, args, 0)
	if err != nil {
		return err
	}
	if len(names) != 1 {
		return errors.New("expected one agent name; see: bro attach --help")
	}
	name := names[0]
	input, inputOK := c.in.(*os.File)
	output, outputOK := c.out.(*os.File)
	if !inputOK || !outputOK || !isTerminal(input) || !isTerminal(output) {
		return errors.New("attach needs a terminal on stdin and stdout; to read the agent's output use: bro tail " + name)
	}
	op, swarm, err := c.selected(ctx)
	if err != nil {
		return err
	}
	agent, err := op.client.Agent(ctx, swarm.ID, name)
	var refusal *clientapi.APIError
	if errors.As(err, &refusal) && refusal.Status == 404 {
		return fmt.Errorf("agent %s not found in %s", name, label(swarm))
	}
	if err != nil {
		return err
	}
	if agent.Phase != "running" {
		return fmt.Errorf("agent %s is %s, not running; start it with: bro start %s", name, dash(agent.Phase), name)
	}
	ticket, err := op.client.TerminalTicket(ctx, swarm.ID, name, *view)
	if err != nil {
		return err
	}
	console := console{input: input, output: output, client: op.client, swarm: swarm.ID, agent: name}
	if *view {
		err = console.raw(func() error { return console.watch(ctx, ticket) })
	} else {
		err = console.raw(func() error { return console.stream(ctx, ticket) })
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "detached from %s\n", name)
	return nil
}

// console — терминал человека, подключённый к одному агенту.
type console struct {
	input, output *os.File
	client        *clientapi.Client
	swarm, agent  string
}

// raw исполняет session в сыром режиме терминала и возвращает его прежним,
// каким бы ни был исход.
func (t console) raw(session func() error) error {
	state, err := term.MakeRaw(int(t.input.Fd()))
	if err != nil {
		return errors.New("could not switch the terminal to raw mode")
	}
	defer func() {
		term.Restore(int(t.input.Fd()), state)
		io.WriteString(t.output, terminalReset+"\r\n")
	}()
	return session()
}

func (t console) size() (columns, rows int) {
	columns, rows, err := term.GetSize(int(t.output.Fd()))
	if err != nil || columns <= 0 || rows <= 0 {
		return 80, 24
	}
	return columns, rows
}

// keys читает ввод человека кусками до Ctrl-] или конца ввода. forward
// получает байты до Ctrl-]; сам Ctrl-] и всё после него агенту не уходят.
func (t console) keys(forward func([]byte) error, stops string) <-chan struct{} {
	detached := make(chan struct{})
	go func() {
		defer close(detached)
		buffer := make([]byte, 4096)
		for {
			n, err := t.input.Read(buffer)
			chunk := buffer[:n]
			if at := strings.IndexAny(string(chunk), stops); at >= 0 {
				forward(chunk[:at])
				return
			}
			if err != nil || forward(chunk) != nil {
				return
			}
		}
	}()
	return detached
}

// stream — живая консоль: вывод как приходит, ввод байт в байт, размер окна
// при входе и на каждый SIGWINCH.
func (t console) stream(ctx context.Context, ticket string) error {
	columns, rows := t.size()
	terminal, err := t.client.DialTerminal(ctx, ticket, columns, rows)
	if err != nil {
		return err
	}
	defer terminal.Close()
	// Консоль рисует на основном экране: после неё курсор уходит в нижнюю
	// строку, чтобы оболочка человека продолжила под картинкой, а не в ней.
	defer func() {
		_, rows := t.size()
		fmt.Fprintf(t.output, "\x1b[%d;1H", rows)
	}()
	resized := make(chan os.Signal, 1)
	signal.Notify(resized, syscall.SIGWINCH)
	defer signal.Stop(resized)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for {
			output, err := terminal.Read()
			if err != nil {
				return
			}
			t.output.Write(output)
		}
	}()
	detached := t.keys(terminal.Write, string(rune(detachKey)))
	for {
		select {
		case <-detached:
			return nil
		case <-closed:
			return nil
		case <-ctx.Done():
			return nil
		case <-resized:
			terminal.Resize(t.size())
		}
	}
}

// watch — режим --view: сервер даёт читателю снимки экрана, а не поток,
// поэтому экран перерисовывается опросом; ввод агенту не уходит.
func (t console) watch(ctx context.Context, ticket string) error {
	detached := t.keys(func([]byte) error { return nil }, string([]rune{detachKey, interrupt}))
	io.WriteString(t.output, "\x1b[?1049h\x1b[?25l")
	ticker := time.NewTicker(viewInterval)
	defer ticker.Stop()
	failures := 0
	for {
		screen, err := t.snapshot(ctx, &ticket)
		switch {
		case err == nil:
			failures = 0
			t.draw(screen)
		case failures >= viewFailures:
			return err
		default:
			failures++
		}
		select {
		case <-detached:
			return nil
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// snapshot читает экран; истёкший ticket (он живёт десять минут) меняется
// на новый, пока агент работает.
func (t console) snapshot(ctx context.Context, ticket *string) (string, error) {
	screen, err := t.client.TerminalSnapshot(ctx, *ticket)
	var refusal *clientapi.APIError
	if !errors.As(err, &refusal) || refusal.Status != 404 {
		return screen, err
	}
	if *ticket, err = t.client.TerminalTicket(ctx, t.swarm, t.agent, true); err != nil {
		return "", err
	}
	return t.client.TerminalSnapshot(ctx, *ticket)
}

// draw рисует снимок с левого верхнего угла и стирает остаток экрана.
func (t console) draw(screen string) {
	columns, rows := t.size()
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	if len(lines) > rows-1 {
		lines = lines[len(lines)-(rows-1):]
	}
	var frame strings.Builder
	frame.WriteString("\x1b[H")
	for _, line := range lines {
		// Длинная строка перенеслась бы и сдвинула экран: режем по ширине.
		if cells := []rune(strings.TrimRight(line, "\r")); len(cells) > columns {
			line = string(cells[:columns])
		}
		frame.WriteString(strings.TrimRight(line, "\r") + "\x1b[K\r\n")
	}
	frame.WriteString("\x1b[J\x1b[7m view of " + t.agent + "; Ctrl-] to leave \x1b[0m")
	io.WriteString(t.output, frame.String())
}
