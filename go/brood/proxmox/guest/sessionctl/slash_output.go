//go:build linux

package sessionctl

import (
	"context"
	"strings"
	"time"
	"unicode"

	"brood.dev/go/brood/proxmox/guest/ptysession"
)

const slashOutputLines = 100
const slashOutputBudget = 5 * time.Second

func (e *engine) slashSnapshot(ctx context.Context, proof string) (string, bool) {
	reply, err := e.call(ctx, ptysession.Request{Action: "tail", Proof: proof, Lines: slashOutputLines})
	return reply.Screen, err == nil && reply.OK
}

func (e *engine) waitSlashOutput(ctx context.Context, proof, command, before string) string {
	ctx, cancel := context.WithTimeout(ctx, slashOutputBudget)
	defer cancel()
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	var previous string
	var stableSince time.Time
	for {
		select {
		case <-ctx.Done():
			return previous
		case <-tick.C:
			screen, ok := e.slashSnapshot(ctx, proof)
			if !ok {
				return ""
			}
			output := newSlashOutput(before, screen, command)
			if output != previous {
				previous, stableSince = output, time.Now()
			} else if output != "" && time.Since(stableSince) >= 600*time.Millisecond {
				return output
			}
		}
	}
}

func newSlashOutput(before, after, command string) string {
	baseline := strings.Split(before, "\n")
	current := strings.Split(after, "\n")
	echo := func(line string) bool {
		line = strings.TrimSpace(line)
		line, ok := strings.CutPrefix(line, "❯")
		return ok && strings.TrimSpace(line) == command
	}
	priorEchoes, echoes, last := 0, 0, -1
	for _, line := range baseline {
		if echo(line) {
			priorEchoes++
		}
	}
	for i, line := range current {
		if echo(line) {
			echoes++
			last = i
		}
	}
	// Без нового эха нельзя отличить ответ команды от предыдущего вывода.
	if echoes <= priorEchoes {
		return ""
	}
	var lines []string
	for _, line := range current[last+1:] {
		clean := strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, line))
		if clean == "" {
			continue
		}
		if strings.HasPrefix(clean, "❯") || strings.Trim(clean, "─━═ ") == "" {
			break
		}
		lines = append(lines, clean)
	}
	return strings.Join(lines, "\n")
}
