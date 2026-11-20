package sshboot

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

// ExitError reports a command that ran and exited with a non-zero status.
type ExitError struct {
	Status int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("sshboot: command exited with status %d", e.Status)
}

// Quote single-quote-escapes one shell argument, so that a POSIX shell reads
// it back as exactly arg. A NUL byte cannot be passed to a shell at all and
// is the caller's concern.
func Quote(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

// Run executes command with the given stdin (may be nil) and returns the exit
// status. Combined stdout and stderr go to out (may be nil) as they arrive;
// writes to out are serialized and none happens after Run returns. A non-zero
// exit is returned as *ExitError carrying the status, not as a generic error.
// Cancelling ctx closes the session channel and returns ctx.Err(); a command
// without a terminal may outlive its channel on the remote side.
//
// Run reads stdin until the command ends, and waits for a Read in progress
// to return or for ctx to end. Error strings never contain the command or
// its input.
func (s *Session) Run(ctx context.Context, command string, stdin io.Reader, out io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if out == nil {
		out = io.Discard
	}
	r := &run{out: out}
	done := make(chan error, 1)
	go func() { done <- r.exec(s.client, command, stdin) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// The worker ends as soon as the peer confirms the close or the
		// connection goes away; it is not awaited, a stuck peer must not
		// hold the caller.
		r.cancel()
		return ctx.Err()
	}
}

// run is the state of one command shared by the worker and the cancellation.
type run struct {
	mu        sync.Mutex
	sess      *ssh.Session
	cancelled bool

	outMu  sync.Mutex
	out    io.Writer
	closed bool
}

// Write passes output to the caller's writer until the run is cancelled.
// stdout and stderr are copied by two goroutines, hence the lock.
func (r *run) Write(p []byte) (int, error) {
	r.outMu.Lock()
	defer r.outMu.Unlock()
	if r.closed {
		return 0, io.ErrClosedPipe
	}
	return r.out.Write(p)
}

// cancel closes the session channel, or leaves a mark for a session that is
// still being opened, and cuts the output off.
func (r *run) cancel() {
	r.mu.Lock()
	r.cancelled = true
	sess := r.sess
	r.mu.Unlock()
	if sess != nil {
		sess.Close()
	}
	r.outMu.Lock()
	r.closed = true
	r.outMu.Unlock()
}

// exec opens a session channel, runs the command and waits for it.
func (r *run) exec(client *ssh.Client, command string, stdin io.Reader) error {
	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("sshboot: open session: %w", err)
	}
	defer sess.Close()
	r.mu.Lock()
	if r.cancelled {
		r.mu.Unlock()
		return context.Canceled
	}
	r.sess = sess
	r.mu.Unlock()

	sess.Stdout = r
	sess.Stderr = r
	var pipe io.WriteCloser
	if stdin != nil {
		if pipe, err = sess.StdinPipe(); err != nil {
			return fmt.Errorf("sshboot: open stdin: %w", err)
		}
	}
	if err := sess.Start(command); err != nil {
		return fmt.Errorf("sshboot: start command: %w", err)
	}
	var fed chan error
	if stdin != nil {
		fed = make(chan error, 1)
		go func() { fed <- feed(pipe, stdin) }()
	}
	err = sess.Wait()
	// Closing the channel fails the pending write of the feeder.
	sess.Close()
	var readErr error
	if fed != nil {
		readErr = <-fed
	}
	var exit *ssh.ExitError
	var missing *ssh.ExitMissingError
	switch {
	case errors.As(err, &exit):
		return &ExitError{Status: exit.ExitStatus()}
	case errors.As(err, &missing):
		return errors.New("sshboot: command ended without an exit status")
	case err != nil:
		return fmt.Errorf("sshboot: run command: %w", err)
	case readErr != nil:
		return fmt.Errorf("sshboot: read stdin: %w", readErr)
	}
	return nil
}

// feed copies stdin to the command and closes its input. It returns only an
// error of the reader: a command that exits without reading all of its input
// is judged by its exit status.
func feed(pipe io.WriteCloser, stdin io.Reader) error {
	defer pipe.Close()
	buf := make([]byte, 32<<10)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			if _, werr := pipe.Write(buf[:n]); werr != nil {
				return nil
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// WriteFile writes data to an absolute path atomically: it streams the bytes
// to a temporary file in the same directory through cat, sets the mode and
// renames it into place. The path must be absolute and clean, without control
// characters; it is shell-quoted before use. An existing directory at path is
// not replaced. mode must be 0600, 0644, 0700 or 0755.
func (s *Session) WriteFile(ctx context.Context, path string, mode os.FileMode, data []byte) error {
	if err := checkRemotePath(path); err != nil {
		return err
	}
	switch mode {
	case 0o600, 0o644, 0o700, 0o755:
	default:
		return fmt.Errorf("sshboot: write %s: mode %04o is not one of 0600, 0644, 0700, 0755", path, uint32(mode))
	}
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return fmt.Errorf("sshboot: write %s: %w", path, err)
	}
	tmp := Quote(path + ".brood-tmp-" + hex.EncodeToString(suffix))
	// mv -T refuses to move the file into a directory that sits at path.
	command := fmt.Sprintf(
		"umask 077 && cat >%[1]s && chmod %04[2]o %[1]s && mv -fT %[1]s %[3]s || { rm -f %[1]s; exit 1; }",
		tmp, uint32(mode), Quote(path))
	var reason complaint
	err := s.Run(ctx, command, bytes.NewReader(data), &reason)
	var exit *ExitError
	if errors.As(err, &exit) {
		if text := reason.String(); text != "" {
			return fmt.Errorf("sshboot: write %s: %s", path, text)
		}
		return fmt.Errorf("sshboot: write %s: %w", path, err)
	}
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return fmt.Errorf("sshboot: write %s: %w", path, err)
	}
	return nil
}

// checkRemotePath validates a path that goes into a shell command.
func checkRemotePath(p string) error {
	if !utf8.ValidString(p) || strings.ContainsFunc(p, unicode.IsControl) {
		return errors.New("sshboot: write: path has control characters or is not UTF-8")
	}
	if !path.IsAbs(p) {
		return fmt.Errorf("sshboot: write %s: path is not absolute", p)
	}
	if path.Clean(p) != p || p == "/" {
		return fmt.Errorf("sshboot: write %s: path is not a clean file path", p)
	}
	return nil
}

// complaint keeps the first bytes of what the remote shell printed, for the
// error text.
type complaint struct {
	buf []byte
}

// maxReason bounds the remote message carried in a WriteFile error.
const maxReason = 512

func (t *complaint) Write(p []byte) (int, error) {
	if room := maxReason - len(t.buf); room > 0 {
		t.buf = append(t.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// String returns the message on one line without control characters.
func (t *complaint) String() string {
	clean := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, string(t.buf))
	return strings.Join(strings.Fields(clean), " ")
}
