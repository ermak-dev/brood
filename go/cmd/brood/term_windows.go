//go:build windows

package main

import "os"

// isTerminal на Windows без golang.org/x/term безопасно считаем false:
// пароль читается как обычная строка с уже напечатанным приглашением.
func isTerminal(f *os.File) bool { return false }

// readPasswordTerminal на Windows не используется: isTerminal всегда false.
func readPasswordTerminal(f *os.File, _ *os.File) (string, error) {
	return readLine(f, "", nil)
}
