package clientapi

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

// Типы сообщений ttyd 1.7.7 (protocol.c): первый байт сообщения. Первое
// сообщение клиента — JSON без типа: его первый байт '{'.
const (
	ttydInput  = '0' // клиент: байты ввода
	ttydResize = '1' // клиент: JSON {"columns":N,"rows":M}
	ttydOutput = '0' // сервер: байты вывода
)

// maxSnapshotBytes — потолок текста снимка экрана.
const maxSnapshotBytes = 1 << 20

// ErrTerminalClosed reports that the server closed the terminal stream.
var ErrTerminalClosed = errors.New("terminal closed by the server")

// TerminalTicket asks the server for a terminal ticket of a running agent and
// returns the ticket path. The session cookie travels only on this request:
// the path itself is the capability for TerminalSnapshot and DialTerminal. A
// read-only ticket allows screen snapshots only, a writable one the stream.
func (c *Client) TerminalTicket(ctx context.Context, swarm, agent string, readOnly bool) (string, error) {
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	path := "/api/swarms/" + url.PathEscape(swarm) + "/proxy/actors/" + url.PathEscape(agent) + "/?mode=" + mode
	resp, err := c.request(ctx, http.MethodGet, path, nil, 0, "", c.http.Timeout)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusSeeOther:
	case resp.StatusCode == http.StatusUnauthorized:
		return "", ErrSessionExpired
	default:
		return "", &APIError{Status: resp.StatusCode, Detail: plainRefusal(resp, "the server refused to open the terminal")}
	}
	location, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || location.Host != "" || !strings.HasPrefix(location.Path, "/actor-view/") || !strings.HasSuffix(location.Path, "/") {
		return "", errors.New("server answered with an unexpected terminal location")
	}
	return location.Path, nil
}

// TerminalSnapshot returns the current screen text of the agent through a
// read-only ticket. The server sends plain text without colours.
func (c *Client) TerminalSnapshot(ctx context.Context, ticket string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.origin+ticket+"?mode=ro", nil)
	if err != nil {
		return "", errors.New("invalid terminal ticket")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("request to %s failed; check network and TLS configuration", c.origin)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", &APIError{Status: resp.StatusCode, Detail: plainRefusal(resp, "terminal snapshot is unavailable")}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSnapshotBytes))
	if err != nil {
		return "", errors.New("terminal snapshot was cut short")
	}
	// Первая строка — пояснение сервера для браузера, не экран агента.
	_, screen, _ := strings.Cut(string(data), "\n")
	return screen, nil
}

// plainRefusal читает текстовый отказ терминальных путей одной строкой.
func plainRefusal(resp *http.Response, fallback string) string {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	text := strings.TrimSpace(strings.Map(func(char rune) rune {
		if char < 32 || (char >= 127 && char <= 159) {
			return ' '
		}
		return char
	}, string(data)))
	if text == "" || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		return fmt.Sprintf("%s (status %d)", fallback, resp.StatusCode)
	}
	return text
}

// Terminal is a live ttyd stream of one agent console.
type Terminal struct {
	conn *websocket.Conn
}

// DialTerminal opens the console stream of a writable ticket with the given
// window size. TLS trust is the one of the HTTP client; no cookie is sent.
func (c *Client) DialTerminal(ctx context.Context, ticket string, columns, rows int) (*Terminal, error) {
	config, err := websocket.NewConfig("ws"+strings.TrimPrefix(c.origin, "http")+ticket+"ws?mode=rw", c.origin)
	if err != nil {
		return nil, errors.New("invalid terminal ticket")
	}
	config.Protocol = []string{"tty"}
	config.TlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if transport, ok := c.http.Transport.(*http.Transport); ok && transport.TLSClientConfig != nil {
		config.TlsConfig = transport.TLSClientConfig.Clone()
	}
	// HTTP-транспорт дописывает в свой конфиг h2; WebSocket открывается
	// запросом HTTP/1.1.
	config.TlsConfig.NextProtos = []string{"http/1.1"}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	conn, err := config.DialContext(ctx)
	if err != nil {
		var cert *tls.CertificateVerificationError
		if errors.As(err, &cert) {
			return nil, errors.New("TLS certificate verification failed; configure a trusted CA")
		}
		return nil, errors.New("could not open the terminal stream; the agent may have stopped")
	}
	terminal := &Terminal{conn: conn}
	// ttyd запускает консоль по первому сообщению: токен пуст, доступ уже
	// проверил сервер по ticket.
	hello, _ := json.Marshal(struct {
		AuthToken string `json:"AuthToken"`
		Columns   int    `json:"columns"`
		Rows      int    `json:"rows"`
	}{"", columns, rows})
	if err := websocket.Message.Send(conn, hello); err != nil {
		conn.Close()
		return nil, errors.New("could not start the terminal stream")
	}
	return terminal, nil
}

// Read returns the next chunk of console output. Window titles and client
// preferences of ttyd are skipped. ErrTerminalClosed ends the stream.
func (t *Terminal) Read() ([]byte, error) {
	for {
		var message []byte
		if err := websocket.Message.Receive(t.conn, &message); err != nil {
			return nil, ErrTerminalClosed
		}
		if len(message) > 1 && message[0] == ttydOutput {
			return message[1:], nil
		}
	}
}

// Write forwards input bytes to the console unchanged.
func (t *Terminal) Write(input []byte) error {
	return t.send(ttydInput, input)
}

// Resize tells the console its new window size.
func (t *Terminal) Resize(columns, rows int) error {
	size, _ := json.Marshal(struct {
		Columns int `json:"columns"`
		Rows    int `json:"rows"`
	}{columns, rows})
	return t.send(ttydResize, size)
}

// Close ends the stream; a blocked Read returns ErrTerminalClosed.
func (t *Terminal) Close() error { return t.conn.Close() }

func (t *Terminal) send(kind byte, payload []byte) error {
	message := make([]byte, 0, len(payload)+1)
	message = append(append(message, kind), payload...)
	if err := websocket.Message.Send(t.conn, message); err != nil {
		return ErrTerminalClosed
	}
	return nil
}
