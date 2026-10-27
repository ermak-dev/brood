package main

import (
	"errors"
	"io"
	"os"

	"golang.org/x/term"
)

func readPasswordInput(stdin io.Reader, _ io.Writer) (string, error) {
	if f, ok := stdin.(*os.File); ok {
		if term.IsTerminal(int(f.Fd())) {
			password, err := term.ReadPassword(int(f.Fd()))
			if err != nil {
				return "", errors.New("could not read password securely from terminal")
			}
			return string(password), nil
		}
		// Если ioctl не распознал символьное устройство как терминал, нельзя
		// переходить к обычному чтению: отказ ioctl мог оставить эхо включённым.
		info, err := f.Stat()
		if err != nil {
			return "", errors.New("could not inspect password input")
		}
		if info.Mode()&os.ModeCharDevice != 0 {
			return "", errors.New("password input is not a supported terminal; use a pipe")
		}
	}
	return readLine(stdin, "", nil)
}
