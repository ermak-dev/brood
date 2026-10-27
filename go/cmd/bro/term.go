// Общий вход чтения пароля: терминал — без эха, поток — построчно.
package main

import (
	"fmt"
	"io"
	"os"
)

func readPasswordInput(stdin io.Reader, stdout io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok && isTerminal(f) {
		return readPasswordTerminal(f, os.Stdout)
	}
	return readLine(stdin, "", nil)
}

var _ = fmt.Fprint
