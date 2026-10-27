// Package kube — namespaced-клиент Kubernetes только для WorkerPool, без
// kubeconfig и kubectl. Отзеркаливает brood/server/projects/kubernetes.py:
// CA проверяет TLS, токен ServiceAccount перечитывается на каждый запрос,
// редиректов нет — Bearer не должен уехать на другой сервер.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Namespace и путь коллекции WorkerPool.
const (
	Namespace = "brood"
	pageSize  = 500
	timeout   = 10 * time.Second
)

// KubeError — отказ с кодом HTTP и безопасной причиной для REST-клиента.
type KubeError struct {
	Status int
	Detail string
}

func (e *KubeError) Error() string { return fmt.Sprintf("%d: %s", e.Status, e.Detail) }

// parseEndpoint проверяет endpoint: ровно HTTPS-origin, без пути, учётных
// данных, запроса и фрагмента.
func parseEndpoint(endpoint string) (*url.URL, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.Path != "" && parsed.Path != "/" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("not an HTTPS origin")
	}
	if port := parsed.Port(); port == "" {
		parsed.Host = parsed.Hostname() + ":443"
	} else if value, portErr := strconv.Atoi(port); portErr != nil || value <= 0 || value > 65535 {
		return nil, errors.New("invalid port")
	}
	return parsed, nil
}

// Client — ограниченный REST-клиент одного ресурса; токен и доверенный корень
// перечитываются на каждый запрос.
type Client struct {
	base      *url.URL
	tokenFile string
	caFile    string
	http      *http.Client
	// timeout — обвязка проверок (по умолчанию — timeout пакета).
	timeout time.Duration
}

// NewClient строит клиент; адрес — HTTPS-origin. Неверный адрес — отказ 503,
// называющий KUBE_API_URL.
func NewClient(endpoint, tokenFile, caFile string) (*Client, error) {
	base, err := parseEndpoint(endpoint)
	if err != nil {
		return nil, &KubeError{Status: 503,
			Detail: "KUBE_API_URL must be an HTTPS origin with a valid port"}
	}
	return &Client{base: base, tokenFile: tokenFile, caFile: caFile,
		http: &http.Client{
			Timeout: timeout,
			// Редиректов нет: Bearer не должен уехать на другой сервер.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}}, nil
}

// request выполняет запрос к API: токен и корень читаются заново, отказы
// транспорта — 503, отказы Kubernetes — с их кодом и причиной. query —
// строка запроса (limit/continue), может быть пустой.
func (c *Client) request(ctx context.Context, method, path, query string, body []byte) (map[string]any, error) {
	token, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return nil, &KubeError{Status: 503,
			Detail: "Kubernetes credentials unavailable: check KUBE_API_TOKEN and KUBE_API_CA"}
	}
	tokenText := strings.TrimSpace(string(token))
	if tokenText == "" {
		return nil, &KubeError{Status: 503, Detail: "KUBE_API_TOKEN is empty"}
	}
	caPEM, err := os.ReadFile(c.caFile)
	if err != nil {
		return nil, &KubeError{Status: 503,
			Detail: "Kubernetes credentials unavailable: check KUBE_API_TOKEN and KUBE_API_CA"}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, &KubeError{Status: 503,
			Detail: "Kubernetes credentials unavailable: check KUBE_API_TOKEN and KUBE_API_CA"}
	}
	target := *c.base
	target.Path = path
	target.RawQuery = query
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), payload)
	if err != nil {
		return nil, &KubeError{Status: 503, Detail: "Kubernetes request could not be built"}
	}
	request.Header.Set("Authorization", "Bearer "+tokenText)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := *c.http
	if c.timeout != 0 {
		client.Timeout = c.timeout
	}
	// Своё TLS-окружение на запрос: ротация корня подхватывается без
	// пересборки клиента.
	client.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool}}
	response, err := client.Do(request)
	if err != nil {
		return nil, &KubeError{Status: 503,
			Detail: "Kubernetes API connection failed; check its address, CA and network policy"}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, &KubeError{Status: 503,
			Detail: "Kubernetes API connection failed; check its address, CA and network policy"}
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil || data == nil {
		return nil, &KubeError{Status: 502,
			Detail: fmt.Sprintf("Kubernetes returned HTTP %d without a JSON response", response.StatusCode)}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		status := 502
		switch response.StatusCode {
		case 400, 401, 403, 404, 409, 422, 429:
			status = response.StatusCode
		}
		detail, ok := data["message"].(string)
		if !ok || detail == "" {
			detail = fmt.Sprintf("Kubernetes returned HTTP %d", response.StatusCode)
		}
		return nil, &KubeError{Status: status, Detail: detail}
	}
	return data, nil
}
