package main

// FEATURE: Password input fails closed and never swallows reader failures.
// SPEC: Потоки поддержаны; неизвестное символьное устройство и ошибка ввода — отказ.
// IMPLEMENTATION: x/term читает терминал без эха; обычное чтение сохраняет ошибки.
// RESULT: IMPLEMENTED — проверены pipe, CRLF, EOF и отказ ввода.
// STATUS: DONE

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

type failingInput struct{}

func (failingInput) Read(p []byte) (int, error) { p[0] = 'x'; return 1, io.ErrUnexpectedEOF }

func TestInputDoesNotSwallowIOFailure(t *testing.T) {
	if _, err := readPasswordInput(failingInput{}, io.Discard); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("want input failure, got %v", err)
	}
}

func TestPasswordInputPipeAndUnsupportedDevice(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "password-secret\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	password, err := readPasswordInput(r, io.Discard)
	if err != nil || password != "password-secret" {
		t.Fatalf("pipe read: %v", err)
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if _, err := readPasswordInput(null, io.Discard); err == nil {
		t.Fatal("unsupported character device accepted")
	}
	for _, input := range []string{"alice\r\npassword-secret\r\n", "alice\npassword-secret"} {
		reader := strings.NewReader(input)
		user, err := readLine(reader, "", nil)
		if err != nil || user != "alice" {
			t.Fatalf("username: %q %v", user, err)
		}
		password, err := readPasswordInput(reader, io.Discard)
		if err != nil || password != "password-secret" {
			t.Fatalf("password read: %v", err)
		}
	}
}
