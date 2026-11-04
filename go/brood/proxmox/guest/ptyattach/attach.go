//go:build linux

// Package ptyattach connects ttyd's terminal to the existing guest PTY host.
package ptyattach

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"brood.dev/go/brood/proxmox/guest/ptysession"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// Run attaches a writable terminal to an existing session. It never creates or
// stops a session, interprets terminal bytes, or closes the caller's files.
// Cancellation and stdin EOF detach successfully and restore terminal state.
func Run(ctx context.Context, path string, stdin, stdout *os.File) (err error) {
	if stdin == nil || stdout == nil {
		return errors.New("attach requires stdin and stdout")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("connect PTY attachment: %w", err)
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopClose()

	columns, rows := terminalSize(stdin)
	if term.IsTerminal(int(stdin.Fd())) {
		state, rawErr := term.MakeRaw(int(stdin.Fd()))
		if rawErr != nil {
			return fmt.Errorf("make attachment terminal raw: %w", rawErr)
		}
		defer func() {
			if restoreErr := term.Restore(int(stdin.Fd()), state); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore attachment terminal: %w", restoreErr))
			}
		}()
	}
	changes := make(chan os.Signal, 1)
	signal.Notify(changes, syscall.SIGWINCH)
	defer signal.Stop(changes)
	if err := encode(conn, ptysession.Request{Action: "attach", Columns: columns, Rows: rows}); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("request PTY attachment: %w", err)
	}
	reader := bufio.NewReaderSize(conn, 64<<10)
	var reply ptysession.Reply
	if err := readMessage(reader, &reply); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("read PTY attachment reply: %w", err)
	}
	if !reply.OK {
		reason := reply.Error
		if reason == "" {
			reason = reply.Outcome
		}
		if reason == "" {
			reason = "host refused writable attachment"
		}
		return fmt.Errorf("PTY attachment refused: %s", reason)
	}
	outputFD := int(stdout.Fd())
	flags, flagErr := unix.FcntlInt(uintptr(outputFD), unix.F_GETFL, 0)
	if flagErr != nil {
		return fmt.Errorf("read attachment output flags: %w", flagErr)
	}
	if flagErr = unix.SetNonblock(outputFD, true); flagErr != nil {
		return fmt.Errorf("configure attachment output: %w", flagErr)
	}
	defer func() {
		if _, restoreErr := unix.FcntlInt(uintptr(outputFD), unix.F_SETFL, flags); restoreErr != nil {
			err = errors.Join(err, fmt.Errorf("restore attachment output flags: %w", restoreErr))
		}
	}()
	if err := writeOutput(ctx, outputFD, reply.Replay); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	inputDone := make(chan error, 1)
	go func() {
		inputErr := forwardInput(ctx, conn, stdin, changes)
		inputDone <- inputErr
		conn.Close()
	}()
	defer func() {
		cancel()
		conn.Close()
		inputErr := <-inputDone
		if err == nil && inputErr != nil {
			err = inputErr
		}
	}()
	for {
		var frame ptysession.Frame
		if readErr := readMessage(reader, &frame); readErr != nil {
			if ctx.Err() != nil || errors.Is(readErr, io.EOF) || errors.Is(readErr, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("read PTY output: %w", readErr)
		}
		switch frame.Type {
		case "output":
			if err := writeOutput(ctx, outputFD, frame.Data); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		case "resize":
			// Подтверждение размера: локальным терминалом уже владеет браузер.
			continue
		case "closed":
			return nil
		case "error":
			return fmt.Errorf("PTY attachment: %s", frame.Data)
		default:
			return fmt.Errorf("unexpected PTY attachment frame %q", frame.Type)
		}
	}
}

func terminalSize(file *os.File) (int, int) {
	columns, rows, err := term.GetSize(int(file.Fd()))
	if err != nil {
		return 80, 24
	}
	return min(240, max(20, columns)), min(80, max(5, rows))
}

func encode(conn net.Conn, value any) error {
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	return ptysession.WriteMessage(conn, value)
}

func forwardInput(ctx context.Context, conn net.Conn, stdin *os.File, changes <-chan os.Signal) error {
	buffer := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-changes:
			columns, rows := terminalSize(stdin)
			if err := encode(conn, ptysession.Frame{Type: "resize", Columns: columns, Rows: rows}); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("send PTY resize: %w", err)
			}
		default:
		}
		fds := []unix.PollFd{{Fd: int32(stdin.Fd()), Events: unix.POLLIN}}
		count, err := unix.Poll(fds, 50)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("poll attachment input: %w", err)
		}
		if count == 0 {
			continue
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return errors.New("attachment input file is closed")
		}
		n, err := stdin.Read(buffer)
		if n > 0 {
			if writeErr := encode(conn, ptysession.Frame{Type: "input", Data: buffer[:n]}); writeErr != nil {
				if ctx.Err() != nil {
					return nil
				}
				return fmt.Errorf("send PTY input: %w", writeErr)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read attachment input: %w", err)
		}
	}
}

func readMessage(reader *bufio.Reader, value any) error {
	var record []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(record)+len(part) > ptysession.MaxMessage {
			return errors.New("PTY message exceeds limit")
		}
		record = append(record, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(record) == ptysession.MaxMessage {
				return errors.New("PTY message exceeds limit without newline")
			}
			continue
		}
		if err != nil {
			if len(record) > 0 && errors.Is(err, io.EOF) {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		return ptysession.ReadMessage(bufio.NewReader(bytes.NewReader(record)), value)
	}
}

func writeOutput(ctx context.Context, fd int, data []byte) error {
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := unix.Write(fd, data)
		if n > 0 {
			data = data[n:]
		}
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if errors.Is(err, unix.EAGAIN) {
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
			if _, pollErr := unix.Poll(fds, 50); pollErr != nil && !errors.Is(pollErr, unix.EINTR) {
				return fmt.Errorf("poll attachment output: %w", pollErr)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("write attachment output: %w", err)
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
