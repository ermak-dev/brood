// Package server — сборка Go-сервера brood: конфигурация из окружения,
// соединение хранилища, аутентификации, прикладного слоя и адаптеров
// Substrate/Kubernetes в один HTTP-сервер, встроенная панель и проба
// готовности. Пакет молчит: данные и ошибки возвращает, печатают только
// командлеты (эпик #27, задача #36).
package server

import (
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Значения по умолчанию: адрес слушателя и путь базы рядом с сервером.
const (
	defaultAddr    = "127.0.0.1:8080"
	defaultDBPath  = "./brood.db"
	secretMinBytes = 32
)

// ConfigError — отказ настройки: называет переменную окружения, без
// которой сервер не может работать (MUST: отказ несёт причину).
type ConfigError struct {
	// Setting — имя настройки (переменная окружения или флаг).
	Setting string
	// Detail — что именно не так со значением.
	Detail string
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("%s: %s", e.Setting, e.Detail)
}

// Config — настройки сервера из переменных окружения. Поля с опцией
// заполнены через lookup; необходимые проверки выполняет New.
type Config struct {
	proxmox *proxmoxRegistry
	// gitRemotes — remotes установки (BROOD_GIT_REMOTES); токены остаются
	// на сервере и не экспортируются.
	gitRemotes gitRemotes
	// DBPath — путь файла SQLite (BROOD_DB_PATH, по умолчанию ./brood.db).
	DBPath string
	// Addr — адрес HTTP-слушателя (BROOD_ADDR, по умолчанию 127.0.0.1:8080).
	Addr string
	// TLSCert и TLSKey — файлы сертификата и ключа (BROOD_TLS_CERT,
	// BROOD_TLS_KEY); оба пусты — чистый HTTP.
	TLSCert string
	TLSKey  string
	// SecureCookies — атрибут Secure у cookie сессии: true при TLS или
	// явном BROOD_SECURE_COOKIES, false только для локальной разработки.
	SecureCookies bool
	// SessionSecret — секрет аутентификации (BROOD_SESSION_SECRET);
	// пустой заменяется случайным на старте процесса.
	SessionSecret []byte
	// AteapiToken, AteapiCA и AteapiEndpoint — параметры клиента ateapi
	// (ATEAPI_TOKEN_FILE, ATEAPI_CA_FILE, ATEAPI_ENDPOINT).
	AteapiToken    string
	AteapiCA       string
	AteapiEndpoint string
	// AtenetRouter is the explicit atenet-router authority (ATENET_ROUTER).
	// Empty disables actor ingress; there is no machine-specific default.
	AtenetRouter string
	// Projected router identity, trust roots, and verified TLS server name.
	AtenetCert       string
	AtenetKey        string
	AtenetCA         string
	AtenetServerName string
	// KubeURL, KubeToken и KubeCA — Kubernetes API (KUBE_API_URL,
	// KUBE_API_TOKEN, KUBE_API_CA).
	KubeURL   string
	KubeToken string
	KubeCA    string
	// WorkerPoolImages и WorkerPoolImage — образы воркеров по классам
	// песочницы, как у Python-сервера.
	WorkerPoolImages string
	WorkerPoolImage  string
	// CLIProxyURL and CLIProxyManagementKey configure the explicit LLM backend.
	// The management credential is server-only and never returned to the panel.
	// RegistryURL is the private OCI registry origin; RegistryImageHost is its worker-facing authority.
	RegistryURL           string
	RegistryImageHost     string
	CLIProxyURL           string
	CLIProxyManagementKey string
	// CLIProxyAPIKey is the server-only model credential (CLIPROXY_API_KEY).
	CLIProxyAPIKey string `json:"-"`
	// CLIProxyCAPEM pins trust for an explicitly configured private proxy.
	CLIProxyCAPEM []byte `json:"-"`
	// ActorLLMURL optionally selects a separate backend without replacing panel routing.
	ActorLLMURL    string
	ActorLLMAPIKey string `json:"-"`
	ActorLLMCAPEM  []byte `json:"-"`
	// artifactsDir — каталог артефактов бутстрапа гостя (BROOD_ARTIFACTS_DIR):
	// свойство машины, без дефолта. Пусто — доставка артефактов не настроена.
	artifactsDir string
}

// String, GoString and Format keep server credentials, including the
// installation Git remotes, out of diagnostic formatting with any verb.
func (Config) String() string               { return "server.Config{redacted}" }
func (c Config) GoString() string           { return c.String() }
func (c Config) Format(f fmt.State, _ rune) { redactFormat(f, c) }

// getenv — источник окружения: интерфейс для тестов.
type getenv func(string) string

// envGet читает переменную, снимая кавычки одинарные и двойные, как
// парсер .env brood: значения там хранятся в кавычках, а не режутся
// по первому «=».
func envGet(get getenv, key string) string {
	value := strings.TrimSpace(get(key))
	if len(value) >= 2 {
		if (value[0] == '\'' && value[len(value)-1] == '\'') ||
			(value[0] == '"' && value[len(value)-1] == '"') {
			value = value[1 : len(value)-1]
		}
	}
	return value
}

// LoadConfig собирает настройки из окружения get. Возвращает ConfigError
// с именем настройки при неверном значении; отсутствие опциональных
// настроек ошибкой не является.
func LoadConfig(get getenv) (Config, error) {
	cfg := Config{
		DBPath:                envGet(get, "BROOD_DB_PATH"),
		Addr:                  envGet(get, "BROOD_ADDR"),
		TLSCert:               envGet(get, "BROOD_TLS_CERT"),
		TLSKey:                envGet(get, "BROOD_TLS_KEY"),
		SessionSecret:         []byte(envGet(get, "BROOD_SESSION_SECRET")),
		AteapiToken:           envGet(get, "ATEAPI_TOKEN_FILE"),
		AteapiCA:              envGet(get, "ATEAPI_CA_FILE"),
		AteapiEndpoint:        envGet(get, "ATEAPI_ENDPOINT"),
		AtenetRouter:          envGet(get, "ATENET_ROUTER"),
		AtenetCert:            envGet(get, "ATENET_CLIENT_CERT_FILE"),
		AtenetKey:             envGet(get, "ATENET_CLIENT_KEY_FILE"),
		AtenetCA:              envGet(get, "ATENET_CA_FILE"),
		AtenetServerName:      envGet(get, "ATENET_SERVER_NAME"),
		KubeURL:               envGet(get, "KUBE_API_URL"),
		KubeToken:             envGet(get, "KUBE_API_TOKEN"),
		KubeCA:                envGet(get, "KUBE_API_CA"),
		WorkerPoolImages:      envGet(get, "WORKER_POOL_IMAGES"),
		WorkerPoolImage:       envGet(get, "WORKER_POOL_IMAGE"),
		RegistryURL:           envGet(get, "BROOD_REGISTRY_URL"),
		RegistryImageHost:     envGet(get, "BROOD_REGISTRY_IMAGE_HOST"),
		CLIProxyAPIKey:        envGet(get, "CLIPROXY_API_KEY"),
		ActorLLMURL:           envGet(get, "BROOD_ACTOR_LLM_URL"),
		ActorLLMAPIKey:        envGet(get, "BROOD_ACTOR_LLM_API_KEY"),
		CLIProxyURL:           envGet(get, "CLIPROXY_URL"),
		CLIProxyManagementKey: envGet(get, "CLIPROXY_MANAGEMENT_KEY"),
	}
	artifactsDir, err := loadGuestArtifactsDir(get)
	if err != nil {
		return cfg, err
	}
	cfg.artifactsDir = artifactsDir
	if cfg.DBPath == "" {
		cfg.DBPath = defaultDBPath
	}
	if cfg.Addr == "" {
		cfg.Addr = defaultAddr
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return cfg, &ConfigError{Setting: "BROOD_TLS_CERT",
			Detail: "and BROOD_TLS_KEY must be set together"}
	}
	tls := cfg.TLSCert != ""
	cfg.SecureCookies = tls || envGet(get, "BROOD_SECURE_COOKIES") == "true"
	if !tls && len(cfg.SessionSecret) > 0 && len(cfg.SessionSecret) < secretMinBytes {
		return cfg, &ConfigError{Setting: "BROOD_SESSION_SECRET",
			Detail: fmt.Sprintf("must be at least %d bytes when HTTP is used", secretMinBytes)}
	}
	registry, err := loadProxmoxRegistry(get)
	if err != nil {
		return cfg, err
	}
	cfg.proxmox = registry
	if cfg.gitRemotes, err = loadGitRemotes(envGet(get, gitRemotesSetting)); err != nil {
		return cfg, err
	}
	for _, trust := range []struct {
		setting string
		value   *[]byte
	}{
		{"CLIPROXY_CA_PEM_B64", &cfg.CLIProxyCAPEM},
		{"BROOD_ACTOR_LLM_CA_PEM_B64", &cfg.ActorLLMCAPEM},
	} {
		if encoded := envGet(get, trust.setting); encoded != "" {
			*trust.value, err = base64.StdEncoding.Strict().DecodeString(encoded)
			if err != nil || !x509.NewCertPool().AppendCertsFromPEM(*trust.value) {
				return cfg, &ConfigError{Setting: trust.setting, Detail: "must encode valid PEM trust certificates"}
			}
		}
	}
	return cfg, cfg.validateAdapters()
}

func (cfg Config) validateAdapters() error {
	if cfg.ActorLLMURL != "" || cfg.ActorLLMAPIKey != "" || len(cfg.ActorLLMCAPEM) > 0 {
		if !proxmoxOrigin(cfg.ActorLLMURL) {
			return &ConfigError{Setting: "BROOD_ACTOR_LLM_URL", Detail: "must be an explicit HTTPS origin"}
		}
		if !proxmoxHeaderValue(cfg.ActorLLMAPIKey) {
			return &ConfigError{Setting: "BROOD_ACTOR_LLM_API_KEY", Detail: "must be a nonempty header-safe secret"}
		}
		if len(cfg.ActorLLMCAPEM) > 0 && !x509.NewCertPool().AppendCertsFromPEM(cfg.ActorLLMCAPEM) {
			return &ConfigError{Setting: "BROOD_ACTOR_LLM_CA_PEM_B64", Detail: "must encode valid PEM trust certificates"}
		}
	}
	if len(cfg.CLIProxyCAPEM) > 0 && !x509.NewCertPool().AppendCertsFromPEM(cfg.CLIProxyCAPEM) {
		return &ConfigError{Setting: "CLIPROXY_CA_PEM_B64", Detail: "must encode valid PEM trust certificates"}
	}
	if err := cfg.validateRegistry(); err != nil {
		return err
	}
	if cfg.CLIProxyURL != "" {
		u, err := url.Parse(cfg.CLIProxyURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return &ConfigError{Setting: "CLIPROXY_URL", Detail: "must be an http(s) origin without credentials, path, query or fragment"}
		}
	}
	for _, c := range cfg.CLIProxyAPIKey {
		if c <= 32 || c >= 127 {
			return &ConfigError{Setting: "CLIPROXY_API_KEY", Detail: "must contain only visible ASCII characters without spaces"}
		}
	}
	if strings.ContainsAny(cfg.CLIProxyManagementKey, "\r\n") {
		return &ConfigError{Setting: "CLIPROXY_MANAGEMENT_KEY", Detail: "must not contain newlines"}
	}
	settings := []struct{ name, value string }{
		{"ATENET_ROUTER", cfg.AtenetRouter},
		{"ATENET_CLIENT_CERT_FILE", cfg.AtenetCert},
		{"ATENET_CLIENT_KEY_FILE", cfg.AtenetKey},
		{"ATENET_CA_FILE", cfg.AtenetCA},
		{"ATENET_SERVER_NAME", cfg.AtenetServerName},
	}
	configured := false
	for _, setting := range settings {
		configured = configured || setting.value != ""
	}
	if !configured {
		return nil
	}
	for _, setting := range settings {
		if setting.value == "" {
			return &ConfigError{Setting: setting.name, Detail: "is required when actor ingress is configured"}
		}
	}
	return nil
}

// LoadConfigEnv читает настройки из окружения процесса.
func LoadConfigEnv() (Config, error) { return LoadConfig(os.Getenv) }

// errNotConfigured — общий отказ необязательного адаптера: называет свои
// настройки, операции через него недоступны (503).
func errNotConfigured(component, settings string) error {
	return &ConfigError{Setting: settings,
		Detail: fmt.Sprintf("%s is not configured", component)}
}

// ErrNotConfiguredAteapi — Substrate-адаптер не настроен.
var ErrNotConfiguredAteapi = errors.New("ateapi is not configured")
