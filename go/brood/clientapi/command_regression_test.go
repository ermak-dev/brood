package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/command"
)

// BUG: Глобальные команды отвергаются клиентом без swarm.
// HYPOTHESIS: Безусловная проверка области расходится с контрактом CLI и сервера.
// SOLUTION: Общая проверка NeedsSwarm разрешает глобальные команды, не стирая выбранную область.
// RESULT: FIXED — loopback HTTP подтверждает POST с пустой областью и сохранение отказа сервера.
// STATUS: FIXED
func TestCommandExecutorGlobalScope(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"project_add", `{"name":"example"}`}, {"project_limit", `{"name":"example"}`},
		{"node", `{}`}, {"node_drain", `{"name":"worker"}`}, {"node_up", `{"name":"worker"}`},
		{"models", `{}`}, {"profile", `{}`}, {"server_diagnose", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, scope := range []string{"", "selected-swarm"} {
				posts := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					posts++
					var body struct {
						Swarm string `json:"swarm_id"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if r.Method != http.MethodPost || r.URL.Path != "/api/commands/"+tc.name || body.Swarm != scope {
						t.Errorf("request=%s %s swarm=%q, want %q", r.Method, r.URL.Path, body.Swarm, scope)
					}
					if scope != "" {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					_, _ = io.WriteString(w, `{"outcome":"ok"}`)
				}))
				c, err := New(server.URL)
				if err != nil {
					t.Fatal(err)
				}
				result, err := (CommandExecutor{Client: c, SwarmID: scope}).Execute(context.Background(), command.Invocation{Name: tc.name, Arguments: json.RawMessage(tc.args)})
				server.Close()
				if scope == "" && (err != nil || result.Outcome != "ok") {
					t.Errorf("global command rejected: result=%+v error=%v", result, err)
				}
				if scope != "" && (err == nil || !strings.Contains(err.Error(), "status 400")) {
					t.Errorf("server scope refusal not preserved: %v", err)
				}
				if posts != 1 {
					t.Errorf("posts=%d, want 1", posts)
				}
			}
		})
	}
}

func TestCommandExecutorActorRequiresScope(t *testing.T) {
	c, _ := New("http://localhost")
	c.http.Transport = commandRoundTripper(func(r *http.Request) (*http.Response, error) {
		t.Error("unscoped actor command posted")
		return nil, errors.New("unexpected request")
	})
	_, err := (CommandExecutor{Client: c}).Execute(context.Background(), command.Invocation{Name: "agents", Arguments: json.RawMessage(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "no swarm selected") {
		t.Fatalf("error=%v", err)
	}
}

type commandRoundTripper func(*http.Request) (*http.Response, error)

func (f commandRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// BUG: send с ожиданием 300 секунд обрывается общим HTTP timeout в 30 секунд.
// HYPOTHESIS: Бюджет HTTP не учитывает валидированный wait_seconds.
// SOLUTION: Для send бюджет растет до wait_seconds + 15 секунд через копию HTTP-клиента.
// RESULT: FIXED — исходящий deadline равен 315 секундам, меньший deadline вызывающего сохранен.
// STATUS: FIXED
func TestCommandExecutorRequestBudget(t *testing.T) {
	for _, tc := range []struct {
		name, args     string
		parent, budget time.Duration
	}{
		{"send", `{"to":"worker","message":"task","wait_seconds":300}`, 0, 315 * time.Second},
		{"send", `{"to":"worker","message":"task","wait_seconds":60}`, 0, 75 * time.Second},
		{"send", `{"to":"worker","message":"task"}`, 0, 30 * time.Second},
		{"agents", `{}`, 0, 30 * time.Second},
		{"send", `{"to":"worker","message":"task","wait_seconds":300}`, 2 * time.Second, 2 * time.Second},
	} {
		t.Run(tc.name+tc.parent.String()+tc.args, func(t *testing.T) {
			c, _ := New("http://localhost")
			c.http.Transport = commandRoundTripper(func(r *http.Request) (*http.Response, error) {
				deadline, ok := r.Context().Deadline()
				remaining := time.Until(deadline)
				if !ok || remaining < tc.budget-time.Second || remaining > tc.budget {
					t.Errorf("outgoing deadline budget=%v (present=%t), want %v", remaining, ok, tc.budget)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"outcome":"ok"}`))}, nil
			})
			ctx := context.Background()
			if tc.parent > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.parent)
				defer cancel()
			}
			_, err := (CommandExecutor{Client: c, SwarmID: "swarm"}).Execute(ctx, command.Invocation{Name: tc.name, Arguments: json.RawMessage(tc.args)})
			if err != nil {
				t.Fatal(err)
			}
			if c.http.Timeout != 30*time.Second {
				t.Fatalf("shared timeout mutated: %v", c.http.Timeout)
			}
		})
	}
}

// BUG: Потеря ответа после POST выглядит как безопасный отказ для повтора.
// HYPOTHESIS: Транспортный отказ теряет неопределенность принятой сервером мутации.
// SOLUTION: Отказ запроса или чтения результата команды предупреждает о неопределенном исходе.
// RESULT: FIXED — таймаут заголовков и тела после POST требует проверки перед повтором.
// STATUS: FIXED
func TestCommandExecutorSubmissionTimeoutUnconfirmed(t *testing.T) {
	for _, stage := range []string{"headers", "body"} {
		t.Run(stage, func(t *testing.T) {
			c, _ := New("http://localhost")
			submitted := false
			c.http.Transport = commandRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPost {
					t.Fatal("expected POST")
				}
				if _, err := io.ReadAll(r.Body); err != nil {
					t.Fatal(err)
				}
				submitted = true
				if stage == "body" {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(commandTimeoutReader{})}, nil
				}
				return nil, context.DeadlineExceeded
			})
			_, err := (CommandExecutor{Client: c, SwarmID: "swarm"}).Execute(context.Background(), command.Invocation{Name: "add", Arguments: json.RawMessage(`{"name":"worker"}`)})
			if !submitted {
				t.Fatal("POST was not submitted")
			}
			if err == nil || !strings.Contains(err.Error(), "unconfirmed") || !strings.Contains(err.Error(), "check before retry") {
				t.Fatalf("unsafe timeout diagnostic: %v", err)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline cause lost: %v", err)
			}
		})
	}
}

type commandTimeoutReader struct{}

func (commandTimeoutReader) Read([]byte) (int, error) { return 0, context.DeadlineExceeded }
