// Package substrate — gRPC-клиент ateapi Substrate для сервера brood.
//
// Отзеркаливает brood/server/ateapi/client.py (контракт): аутентификация —
// projected ServiceAccount токен с audience api.ate-system.svc, перечитываемый
// с диска перед каждым RPC; TLS — доверенный корень из файла; нестандартный
// endpoint (port-forward разработчика) сверяет имя сервиса, на которое
// выписан сертификат ateapi. Библиотека молчит: отказы — ошибки-значения.
package substrate

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Audience ateapi проверяет ровно этот; endpoint по умолчанию из него же.
// PolicyName — имя синглтон-политик доступа, другого ateapi не примет.
const (
	Audience   = "api.ate-system.svc"
	Endpoint   = Audience + ":443"
	PolicyName = "default"
)

// forHTTP переводит коды grpc в HTTP; причина — всегда message ateapi: у
// одного кода разные поводы (FAILED_PRECONDITION — и «atespace не пуст», и
// «atespace нет»). Остальные коды — 502.
var forHTTP = map[codes.Code]int{
	codes.NotFound:           404,
	codes.AlreadyExists:      409,
	codes.Aborted:            409,
	codes.FailedPrecondition: 409,
	codes.InvalidArgument:    400,
	codes.PermissionDenied:   403,
	codes.Unauthenticated:    403,
	codes.Unavailable:        503,
}

// APIError — отказ, готовый к HTTP-ответу: статус и причина строкой, без
// трассировки. Прокси Substrate отдаёт его панели и CLI как есть.
type APIError struct {
	Status int
	Detail string
}

func (e *APIError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Detail) }

// tokenSource выдаёт заголовок authorization из файла на каждый вызов:
// kubelet ротирует projected-токен, канал пересобирать не нужно.
type tokenSource struct{ path string }

func (s tokenSource) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, &APIError{Status: 503,
			Detail: "ateapi is not configured: ATEAPI_TOKEN must point at the projected ServiceAccount token"}
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return nil, &APIError{Status: 503, Detail: "ATEAPI_TOKEN is empty"}
	}
	return map[string]string{"authorization": "Bearer " + token}, nil
}

// RequireTransportSecurity — токен едет только по TLS.
func (tokenSource) RequireTransportSecurity() bool { return true }

// Client — Control-клиент ateapi. Токен и доверенный корень — пути, чтобы
// подменять их в проверках.
type Client struct {
	conn *grpc.ClientConn
}

// New строит клиент. Пустые tokenFile/caFile — нет projected-тома (локальная
// копия без кластера): отказ 503 называет настройки. Пустой endpoint —
// Endpoint. Остальные dialOpts — обвязка проверок (bufconn);grpc.NewClient
// сам по себе не подключается, первый RPC и вскроет недоступность.
func New(tokenFile, caFile, endpoint string, dialOpts ...grpc.DialOption) (*Client, error) {
	if tokenFile == "" || caFile == "" {
		return nil, &APIError{Status: 503,
			Detail: "ateapi is not configured: ATEAPI_TOKEN and ATEAPI_CA point the server at its projected ServiceAccount token and ClusterTrustBundle"}
	}
	pool, err := loadCA(caFile)
	if err != nil {
		return nil, err
	}
	if endpoint == "" {
		endpoint = Endpoint
	}
	// Другой адрес — port-forward разработчика: TLS всё равно сверяет имя
	// сервиса, на которое выписан сертификат ateapi.
	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
			RootCAs: pool, ServerName: Audience})),
		grpc.WithPerRPCCredentials(tokenSource{path: tokenFile}),
	}, dialOpts...)
	conn, err := grpc.NewClient(endpoint, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", endpoint, err)
	}
	return &Client{conn: conn}, nil
}

// loadCA читает доверенный корень при создании канала: смена корня — редкая
// операция canary, канал пересобирается.
func loadCA(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, &APIError{Status: 503,
			Detail: "ateapi is not configured: ATEAPI_CA must point at the ClusterTrustBundle root certificate"}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, &APIError{Status: 503, Detail: "ATEAPI_CA is not a valid certificate bundle"}
	}
	return pool, nil
}

// Close освобождает канал.
func (c *Client) Close() { c.conn.Close() }

// stub создаёт Control-заглушку канала.
func (c *Client) stub() ateapi.ControlClient { return ateapi.NewControlClient(c.conn) }

// mapGRPC переводит отказ gRPC в APIError: код — в HTTP, причина — message
// ateapi, не имя кода. Отказ per-RPC credentials (токен не прочитан) уже
// APIError — проходит насквозь.
func mapGRPC(err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	st, ok := status.FromError(err)
	if !ok {
		return &APIError{Status: 503, Detail: err.Error()}
	}
	code, mapped := forHTTP[st.Code()]
	if !mapped {
		code = 502
	}
	detail := st.Message()
	if detail == "" {
		detail = st.Code().String()
	}
	return &APIError{Status: code, Detail: detail}
}

// ─── protojson ─────────────────────────────────────────────────────────────

// jsonOpts: DiscardUnknown=false — неизвестное поле или неверный тип
// разбираемого тела — отказ, панель получает 400 с причиной, а не молчаливую
// потерю поля.
var (
	marshalOpts   = protojson.MarshalOptions{}
	unmarshalOpts = protojson.UnmarshalOptions{DiscardUnknown: false}
)

// Marshal печатает сообщение каноническим protojson (lowerCamelCase): его же
// читают kubectl ate и демо-шаблоны Substrate.
func Marshal(message proto.Message) ([]byte, error) {
	data, err := marshalOpts.Marshal(message)
	if err != nil {
		return nil, &APIError{Status: 400,
			Detail: fmt.Sprintf("cannot serialize %s: %v", nameOf(message), err)}
	}
	return data, nil
}

// Unmarshal разбирает тело запроса в сообщение; не-объект, неизвестное поле
// или неверный тип — 400 с причиной.
func Unmarshal(data []byte, message proto.Message) error {
	if !strings.HasPrefix(strings.TrimSpace(string(data)), "{") {
		return &APIError{Status: 400,
			Detail: fmt.Sprintf("the body must be a JSON object (%s)", nameOf(message))}
	}
	if err := unmarshalOpts.Unmarshal(data, message); err != nil {
		return &APIError{Status: 400,
			Detail: fmt.Sprintf("invalid %s: %v", nameOf(message), err)}
	}
	return nil
}

// nameOf — имя сообщения прото для причин.
func nameOf(message proto.Message) string {
	if message == nil {
		return "message"
	}
	return string(message.ProtoReflect().Descriptor().Name())
}

// walk обходит все страницы List-RPC: page возвращает страницу и токен
// следующей (потолок страницы ateapi — 1000); пустой токен — конец.
func walk[T any](page func(token string) (items []T, next string, err error)) ([]T, error) {
	var out []T
	for token := ""; ; {
		items, next, err := page(token)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if next == "" {
			return out, nil
		}
		token = next
	}
}

// ref — ссылка на ресурс; atespace пуст у глобальных (Atespace, Worker).
func ref(name, atespace string) *ateapi.ObjectRef {
	return &ateapi.ObjectRef{Atespace: atespace, Name: name}
}

// ─── Atespace ──────────────────────────────────────────────────────────────

// ListAtespaces обходит страницы и собирает все atespace.
func (c *Client) ListAtespaces(ctx context.Context) ([]*ateapi.Atespace, error) {
	return walk(func(token string) ([]*ateapi.Atespace, string, error) {
		response, err := c.stub().ListAtespaces(ctx, &ateapi.ListAtespacesRequest{
			Options: &ateapi.ListOptions{PageToken: token}})
		if err != nil {
			return nil, "", mapGRPC(err)
		}
		return response.GetAtespaces(), response.GetNextPageToken(), nil
	})
}

// GetAtespace возвращает atespace по имени.
func (c *Client) GetAtespace(ctx context.Context, name string) (*ateapi.Atespace, error) {
	out, err := c.stub().GetAtespace(ctx, &ateapi.GetAtespaceRequest{Atespace: ref(name, "")})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// CreateAtespace создаёт atespace из сообщения.
func (c *Client) CreateAtespace(ctx context.Context, atespace *ateapi.Atespace) (*ateapi.Atespace, error) {
	out, err := c.stub().CreateAtespace(ctx, &ateapi.CreateAtespaceRequest{Atespace: atespace})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// DeleteAtespace удаляет atespace по имени; непустую ateapi не удалит.
func (c *Client) DeleteAtespace(ctx context.Context, name string, uid ...string) error {
	request := &ateapi.DeleteAtespaceRequest{Atespace: ref(name, "")}
	if len(uid) > 0 {
		request.Options = &ateapi.DeleteOptions{Uid: uid[0]}
	}
	_, err := c.stub().DeleteAtespace(ctx, request)
	if err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			return fmt.Errorf("%w: %w", ErrAtespaceNotEmpty, mapGRPC(err))
		}
		return mapGRPC(err)
	}
	return nil
}

// ─── Actor ─────────────────────────────────────────────────────────────────

// ListActors собирает всех акторов atespace по страницам.
func (c *Client) ListActors(ctx context.Context, atespace string) ([]*ateapi.Actor, error) {
	return walk(func(token string) ([]*ateapi.Actor, string, error) {
		response, err := c.stub().ListActors(ctx, &ateapi.ListActorsRequest{
			Atespace: atespace, Options: &ateapi.ListOptions{PageToken: token}})
		if err != nil {
			return nil, "", mapGRPC(err)
		}
		return response.GetActors(), response.GetNextPageToken(), nil
	})
}

// GetActor возвращает актора atespace по имени.
func (c *Client) GetActor(ctx context.Context, atespace, name string) (*ateapi.Actor, error) {
	out, err := c.stub().GetActor(ctx, &ateapi.GetActorRequest{Actor: ref(name, atespace)})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// CreateActor создаёт актора из сообщения.
func (c *Client) CreateActor(ctx context.Context, actor *ateapi.Actor) (*ateapi.Actor, error) {
	out, err := c.stub().CreateActor(ctx, &ateapi.CreateActorRequest{Actor: actor})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// UpdateActor заменяет изменяемые поля (шаблон, workerSelector); uid и version
// в metadata — предусловие: ateapi откажет ABORTED, если актор менялся.
func (c *Client) UpdateActor(ctx context.Context, actor *ateapi.Actor) (*ateapi.Actor, error) {
	out, err := c.stub().UpdateActor(ctx, &ateapi.UpdateActorRequest{Actor: actor})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// SuspendActor приостанавливает актора и возвращает его же.
func (c *Client) SuspendActor(ctx context.Context, atespace, name string) (*ateapi.Actor, error) {
	response, err := c.stub().SuspendActor(ctx, &ateapi.SuspendActorRequest{Actor: ref(name, atespace)})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return response.GetActor(), nil
}

// ResumeActor возобновляет актора и возвращает его же.
func (c *Client) ResumeActor(ctx context.Context, atespace, name string) (*ateapi.Actor, error) {
	response, err := c.stub().ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref(name, atespace)})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return response.GetActor(), nil
}

// DeleteActor удаляет актора.
func (c *Client) DeleteActor(ctx context.Context, atespace, name string) error {
	_, err := c.stub().DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref(name, atespace)})
	if err != nil {
		return mapGRPC(err)
	}
	return nil
}

// ─── Egress-политика актора ────────────────────────────────────────────────

// GetActorEgressPolicy возвращает синглтон egress-политики актора.
func (c *Client) GetActorEgressPolicy(ctx context.Context, atespace, actor string) (*ateapi.EgressPolicy, error) {
	out, err := c.stub().GetActorEgressPolicy(ctx, &ateapi.GetActorEgressPolicyRequest{
		Actor: ref(actor, atespace), Name: PolicyName})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// CreateActorEgressPolicy заводит egress-политику актора.
func (c *Client) CreateActorEgressPolicy(ctx context.Context, atespace, actor string, policy *ateapi.EgressPolicy) (*ateapi.EgressPolicy, error) {
	out, err := c.stub().CreateActorEgressPolicy(ctx, &ateapi.CreateActorEgressPolicyRequest{
		Actor: ref(actor, atespace), EgressPolicy: policy})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// UpdateActorEgressPolicy заменяет egress-политику актора; uid и version —
// предусловие.
func (c *Client) UpdateActorEgressPolicy(ctx context.Context, atespace, actor string, policy *ateapi.EgressPolicy) (*ateapi.EgressPolicy, error) {
	out, err := c.stub().UpdateActorEgressPolicy(ctx, &ateapi.UpdateActorEgressPolicyRequest{
		Actor: ref(actor, atespace), EgressPolicy: policy})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// DeleteActorEgressPolicy удаляет egress-политику актора.
func (c *Client) DeleteActorEgressPolicy(ctx context.Context, atespace, actor string) error {
	_, err := c.stub().DeleteActorEgressPolicy(ctx, &ateapi.DeleteActorEgressPolicyRequest{
		Actor: ref(actor, atespace), Name: PolicyName})
	if err != nil {
		return mapGRPC(err)
	}
	return nil
}

// ─── ActorTemplate: неизменяем, Update в ateapi нет ────────────────────────

// ListActorTemplates собирает шаблоны акторов atespace по страницам.
func (c *Client) ListActorTemplates(ctx context.Context, atespace string) ([]*ateapi.ActorTemplate, error) {
	return walk(func(token string) ([]*ateapi.ActorTemplate, string, error) {
		response, err := c.stub().ListActorTemplates(ctx, &ateapi.ListActorTemplatesRequest{
			Atespace: atespace, Options: &ateapi.ListOptions{PageToken: token}})
		if err != nil {
			return nil, "", mapGRPC(err)
		}
		return response.GetActorTemplates(), response.GetNextPageToken(), nil
	})
}

// GetActorTemplate возвращает шаблон по имени.
func (c *Client) GetActorTemplate(ctx context.Context, atespace, name string) (*ateapi.ActorTemplate, error) {
	out, err := c.stub().GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{
		ActorTemplate: ref(name, atespace)})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// CreateActorTemplate создаёт шаблон из сообщения.
func (c *Client) CreateActorTemplate(ctx context.Context, template *ateapi.ActorTemplate) (*ateapi.ActorTemplate, error) {
	out, err := c.stub().CreateActorTemplate(ctx, &ateapi.CreateActorTemplateRequest{ActorTemplate: template})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// DeleteActorTemplate удаляет шаблон; правка — новый шаблон.
func (c *Client) DeleteActorTemplate(ctx context.Context, atespace, name string) error {
	_, err := c.stub().DeleteActorTemplate(ctx, &ateapi.DeleteActorTemplateRequest{
		ActorTemplate: ref(name, atespace)})
	if err != nil {
		return mapGRPC(err)
	}
	return nil
}

// ─── Worker: зеркало пода, пишет ate-controller ────────────────────────────

// ListWorkers собирает всех воркеров установки по страницам.
func (c *Client) ListWorkers(ctx context.Context) ([]*ateapi.Worker, error) {
	return walk(func(token string) ([]*ateapi.Worker, string, error) {
		response, err := c.stub().ListWorkers(ctx, &ateapi.ListWorkersRequest{
			Options: &ateapi.ListOptions{PageToken: token}})
		if err != nil {
			return nil, "", mapGRPC(err)
		}
		return response.GetWorkers(), response.GetNextPageToken(), nil
	})
}

// GetWorker возвращает воркера по имени.
func (c *Client) GetWorker(ctx context.Context, name string) (*ateapi.Worker, error) {
	out, err := c.stub().GetWorker(ctx, &ateapi.GetWorkerRequest{Worker: ref(name, "")})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// DrainWorker помечает воркера terminating и возвращает его же.
func (c *Client) DrainWorker(ctx context.Context, name string) (*ateapi.Worker, error) {
	response, err := c.stub().DrainWorker(ctx, &ateapi.DrainWorkerRequest{Worker: ref(name, "")})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return response.GetWorker(), nil
}

// ─── AccessPolicy: одна на скоуп, имя default ──────────────────────────────

// GetGlobalAccessPolicy возвращает глобальную политику доступа.
func (c *Client) GetGlobalAccessPolicy(ctx context.Context) (*ateapi.AccessPolicy, error) {
	out, err := c.stub().GetGlobalAccessPolicy(ctx, &ateapi.GetGlobalAccessPolicyRequest{Name: PolicyName})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// CreateGlobalAccessPolicy заводит глобальную политику.
func (c *Client) CreateGlobalAccessPolicy(ctx context.Context, policy *ateapi.AccessPolicy) (*ateapi.AccessPolicy, error) {
	out, err := c.stub().CreateGlobalAccessPolicy(ctx, &ateapi.CreateGlobalAccessPolicyRequest{AccessPolicy: policy})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// UpdateGlobalAccessPolicy заменяет глобальную политику; uid и version —
// предусловие.
func (c *Client) UpdateGlobalAccessPolicy(ctx context.Context, policy *ateapi.AccessPolicy) (*ateapi.AccessPolicy, error) {
	out, err := c.stub().UpdateGlobalAccessPolicy(ctx, &ateapi.UpdateGlobalAccessPolicyRequest{AccessPolicy: policy})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// GetAtespaceAccessPolicy возвращает политику доступа atespace.
func (c *Client) GetAtespaceAccessPolicy(ctx context.Context, atespace string) (*ateapi.AccessPolicy, error) {
	out, err := c.stub().GetAtespaceAccessPolicy(ctx, &ateapi.GetAtespaceAccessPolicyRequest{
		Atespace: ref(atespace, ""), Name: PolicyName})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// CreateAtespaceAccessPolicy заводит политику доступа atespace.
func (c *Client) CreateAtespaceAccessPolicy(ctx context.Context, atespace string, policy *ateapi.AccessPolicy) (*ateapi.AccessPolicy, error) {
	out, err := c.stub().CreateAtespaceAccessPolicy(ctx, &ateapi.CreateAtespaceAccessPolicyRequest{
		Atespace: ref(atespace, ""), AccessPolicy: policy})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// UpdateAtespaceAccessPolicy заменяет политику доступа atespace.
func (c *Client) UpdateAtespaceAccessPolicy(ctx context.Context, atespace string, policy *ateapi.AccessPolicy) (*ateapi.AccessPolicy, error) {
	out, err := c.stub().UpdateAtespaceAccessPolicy(ctx, &ateapi.UpdateAtespaceAccessPolicyRequest{
		Atespace: ref(atespace, ""), AccessPolicy: policy})
	if err != nil {
		return nil, mapGRPC(err)
	}
	return out, nil
}

// DeleteAtespaceAccessPolicy удаляет политику доступа atespace; глобальную
// ateapi не удаляет.
func (c *Client) DeleteAtespaceAccessPolicy(ctx context.Context, atespace string) error {
	_, err := c.stub().DeleteAtespaceAccessPolicy(ctx, &ateapi.DeleteAtespaceAccessPolicyRequest{
		Atespace: ref(atespace, ""), Name: PolicyName})
	if err != nil {
		return mapGRPC(err)
	}
	return nil
}
