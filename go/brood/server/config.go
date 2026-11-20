// Package server — сборка Go-сервера brood: конфигурация из окружения,
// соединение хранилища, аутентификации, прикладного слоя и адаптеров
// Substrate/Kubernetes в один HTTP-сервер, встроенная панель и проба
// готовности. Пакет молчит: данные и ошибки возвращает, печатают только
// командлеты (эпик #27, задача #36).
package server

import (
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"strings"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/gitidentity"
	"brood.dev/go/brood/telegram"
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
	SessionSecret         []byte
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
	// ActorGitDomain — домен адреса Git-идентичности акторов
	// (BROOD_ACTOR_GIT_DOMAIN, #249): свойство установки, без дефолта.
	// Пусто — гость идентичность не получает и не пишет.
	ActorGitDomain string
	// ActorModels — модели Claude Code акторов (BROOD_ACTOR_MODEL основная,
	// BROOD_ACTOR_SMALL_MODEL быстрых задач, #263): свойство установки, без
	// дефолта, каждая сама по себе. Пусто — модель выбирает Claude Code.
	ActorModels actormodel.Models
	// artifactsDir — каталог артефактов бутстрапа гостя (BROOD_ARTIFACTS_DIR):
	// свойство машины, без дефолта. Пусто — доставка артефактов не настроена.
	artifactsDir string
	// TelegramAPI — origin Bot API (BROOD_TELEGRAM_API, #281): по умолчанию
	// публичный telegram.DefaultBaseURL; HTTPS или HTTP на loopback для
	// локального Bot API. Им пользуются бот установки и свои боты.
	TelegramAPI string
	// telegramBotToken — токен бота установки (BROOD_TELEGRAM_BOT_TOKEN),
	// секрет установки: только .env и секрет CI, не в логи, ответы API и
	// гостевое окружение. Пусто — бота установки нет.
	telegramBotToken string
	// PanelOrigin — origin панели, каким его открывает пользователь
	// (BROOD_PANEL_ORIGIN, #282): уведомления Telegram ссылаются на объект
	// панели по нему. Свойство установки, без дефолта; пусто — уведомления
	// без ссылки.
	PanelOrigin string
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
		CLIProxyAPIKey:        envGet(get, "CLIPROXY_API_KEY"),
		ActorLLMURL:           envGet(get, "BROOD_ACTOR_LLM_URL"),
		ActorLLMAPIKey:        envGet(get, "BROOD_ACTOR_LLM_API_KEY"),
		ActorGitDomain:        envGet(get, "BROOD_ACTOR_GIT_DOMAIN"),
		ActorModels:           actormodel.Models{Model: envGet(get, "BROOD_ACTOR_MODEL"), Small: envGet(get, "BROOD_ACTOR_SMALL_MODEL")},
		CLIProxyURL:           envGet(get, "CLIPROXY_URL"),
		CLIProxyManagementKey: envGet(get, "CLIPROXY_MANAGEMENT_KEY"),
		TelegramAPI:           envGet(get, telegramAPISetting),
		telegramBotToken:      envGet(get, telegramBotTokenSetting),
	}
	panel, err := loadPanelOrigin(envGet(get, panelOriginSetting))
	if err != nil {
		return cfg, err
	}
	cfg.PanelOrigin = panel
	if cfg.TelegramAPI == "" {
		cfg.TelegramAPI = telegram.DefaultBaseURL
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
	if cfg.ActorGitDomain != "" && !gitidentity.ValidDomain(cfg.ActorGitDomain) {
		return &ConfigError{Setting: "BROOD_ACTOR_GIT_DOMAIN", Detail: "must be a lowercase DNS name"}
	}
	for _, model := range []struct{ setting, name string }{
		{"BROOD_ACTOR_MODEL", cfg.ActorModels.Model},
		{"BROOD_ACTOR_SMALL_MODEL", cfg.ActorModels.Small},
	} {
		if model.name != "" && !actormodel.ValidName(model.name) {
			return &ConfigError{Setting: model.setting, Detail: "must be at most 256 printable ASCII characters without spaces"}
		}
	}
	if err := cfg.validateTelegram(); err != nil {
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
	return nil
}

// Настройки Telegram (#281): origin Bot API и токен бота установки.
const (
	telegramAPISetting      = "BROOD_TELEGRAM_API"
	telegramBotTokenSetting = "BROOD_TELEGRAM_BOT_TOKEN"
)

// telegramProbeToken — токен формы BotFather для проверки одного origin
// клиентом: без бота установки токена нет, а origin нужен и своим ботам.
const telegramProbeToken = "0:probe"

// validateTelegram проверяет origin Bot API и токен бота установки тем же
// клиентом, который их возьмёт. Отказ называет настройку, но не значение.
func (cfg Config) validateTelegram() error {
	client, err := telegram.NewClient(telegramProbeToken, telegram.Config{BaseURL: cfg.TelegramAPI})
	if err != nil {
		return &ConfigError{Setting: telegramAPISetting, Detail: "must be an HTTPS origin, or an HTTP origin on a loopback address, without path, query or credentials"}
	}
	client.Close()
	if cfg.telegramBotToken == "" {
		return nil
	}
	client, err = telegram.NewClient(cfg.telegramBotToken, telegram.Config{BaseURL: cfg.TelegramAPI})
	if err != nil {
		return &ConfigError{Setting: telegramBotTokenSetting, Detail: "must be a bot token as BotFather issues it: <bot id>:<secret>"}
	}
	client.Close()
	return nil
}

// panelOriginSetting — origin панели для ссылок уведомлений (#282).
const panelOriginSetting = "BROOD_PANEL_ORIGIN"

// loadPanelOrigin проверяет origin панели: http(s) без credentials, path,
// query и fragment; завершающий "/" снимается. Пусто — ссылок нет.
func loadPanelOrigin(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", &ConfigError{Setting: panelOriginSetting, Detail: "must be an http(s) origin without credentials, path, query or fragment"}
	}
	return u.Scheme + "://" + u.Host, nil
}

// LoadConfigEnv читает настройки из окружения процесса.
func LoadConfigEnv() (Config, error) { return LoadConfig(os.Getenv) }

// errNotConfigured — общий отказ необязательного адаптера: называет свои
// настройки, операции через него недоступны (503).
func errNotConfigured(component, settings string) error {
	return &ConfigError{Setting: settings,
		Detail: fmt.Sprintf("%s is not configured", component)}
}
