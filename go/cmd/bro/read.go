// Построчное чтение без упреждающей буферизации: username не забирает
// пароль из того же потока. Терминальный пароль читает golang.org/x/term.
package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// readLine читает строку до '\n' посимвольно: без буферизации, чтобы
// подряд идущие чтения (username, затем пароль) не съедали ввод друг
// друга, а приглашения оставались интерактивными.
func readLine(in io.Reader, prompt string, out io.Writer) (string, error) {
	if out != nil && prompt != "" {
		fmt.Fprint(out, prompt)
	}
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return strings.TrimRight(sb.String(), "\r"), nil
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return "", fmt.Errorf("read input: %w", err)
			}
			if sb.Len() > 0 {
				return strings.TrimRight(sb.String(), "\r"), nil
			}
			return "", errors.New("input closed")
		}
	}
}
