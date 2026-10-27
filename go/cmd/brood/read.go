// Построчное чтение без буферизации и пароль без эха: тот же подход,
// что cmd/bro/term_*.go, скопированный, а не импортированный — командные
// бинарники не зависят друг от друга.
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
			if sb.Len() > 0 {
				return strings.TrimRight(sb.String(), "\r"), nil
			}
			return "", errors.New("input closed")
		}
	}
}
