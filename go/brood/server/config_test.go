// Проверки настроек: значения по умолчанию, съём кавычек .env, отказы
// с именем настройки.
package server

import (
	"errors"
	"testing"
)

func TestConfigRouterCredentials(t *testing.T) {
	values := map[string]string{
		"ATENET_ROUTER":           "router.test:443",
		"ATENET_CLIENT_CERT_FILE": "/projected/tls.crt",
		"ATENET_CLIENT_KEY_FILE":  "/projected/tls.key",
		"ATENET_CA_FILE":          "/projected/ca.crt",
		"ATENET_SERVER_NAME":      "router.test",
	}
	cfg, err := LoadConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AtenetRouter != values["ATENET_ROUTER"] || cfg.AtenetCert != values["ATENET_CLIENT_CERT_FILE"] || cfg.AtenetKey != values["ATENET_CLIENT_KEY_FILE"] || cfg.AtenetCA != values["ATENET_CA_FILE"] || cfg.AtenetServerName != values["ATENET_SERVER_NAME"] {
		t.Fatalf("router settings not loaded: %+v", cfg)
	}
}

func TestConfigIncompleteRouterDenied(t *testing.T) {
	_, err := LoadConfig(func(key string) string {
		if key == "ATENET_ROUTER" {
			return "router.test:443"
		}
		return ""
	})
	if err == nil {
		t.Fatal("router without projected identity was accepted")
	}
}

// TestConfigDefaults — пустое окружение даёт путь базы и адрес.
func TestConfigDefaults(t *testing.T) {
	cfg, err := LoadConfig(func(string) string { return "" })
	if err != nil {
		t.Fatalf("empty env: %v", err)
	}
	if cfg.DBPath != "./brood.db" {
		t.Fatalf("default DBPath = %q", cfg.DBPath)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Fatalf("default Addr = %q", cfg.Addr)
	}
	if cfg.SecureCookies {
		t.Fatal("cookies must not be secure without TLS")
	}
}

// TestConfigQuotes — кавычки значений .env снимаются, как парсер brood.
func TestConfigQuotes(t *testing.T) {
	values := map[string]string{
		"BROOD_ADDR":           "'127.0.0.1:9000'",
		"BROOD_DB_PATH":        `"/tmp/brood test.db"`,
		"BROOD_SESSION_SECRET": "'0123456789abcdef0123456789abcdef'",
	}
	cfg, err := LoadConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("quoted env: %v", err)
	}
	if cfg.Addr != "127.0.0.1:9000" {
		t.Fatalf("Addr = %q", cfg.Addr)
	}
	if cfg.DBPath != "/tmp/brood test.db" {
		t.Fatalf("DBPath = %q", cfg.DBPath)
	}
	if string(cfg.SessionSecret) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("secret = %q", cfg.SessionSecret)
	}
}

// TestConfigTLSPair — только один из сертификата и ключа — отказ
// с именами обеих настроек.
func TestConfigTLSPair(t *testing.T) {
	values := map[string]string{"BROOD_TLS_CERT": "/tmp/cert.pem"}
	_, err := LoadConfig(func(key string) string { return values[key] })
	if err == nil {
		t.Fatal("half TLS pair must fail")
	}
	cfgErr, ok := err.(*ConfigError)
	if !ok {
		t.Fatalf("error type %T", err)
	}
	if cfgErr.Setting != "BROOD_TLS_CERT" || cfgErr.Detail == "" {
		t.Fatalf("error %v", cfgErr)
	}
}

// TestConfigSecureCookies — TLS и явный флаг включают Secure.
func TestConfigSecureCookies(t *testing.T) {
	values := map[string]string{"BROOD_TLS_CERT": "c", "BROOD_TLS_KEY": "k"}
	cfg, err := LoadConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("tls env: %v", err)
	}
	if !cfg.SecureCookies {
		t.Fatal("TLS must imply secure cookies")
	}
	values = map[string]string{"BROOD_SECURE_COOKIES": "true"}
	cfg, err = LoadConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("flag env: %v", err)
	}
	if !cfg.SecureCookies {
		t.Fatal("explicit flag must enable secure cookies")
	}
}

// TestConfigShortSecretHTTP — короткий секрет без TLS — отказ с именем
// настройки.
func TestConfigShortSecretHTTP(t *testing.T) {
	values := map[string]string{"BROOD_SESSION_SECRET": "short"}
	_, err := LoadConfig(func(key string) string { return values[key] })
	if err == nil {
		t.Fatal("short secret over HTTP must fail")
	}
	if _, ok := err.(*ConfigError); !ok {
		t.Fatalf("error type %T", err)
	}
}

// TestConfigAteapi — переменные адаптера приходят как есть.
func TestConfigAteapi(t *testing.T) {
	values := map[string]string{
		"ATEAPI_TOKEN_FILE": "/var/run/ateapi/token",
		"ATEAPI_CA_FILE":    "/var/run/ateapi/ca.crt",
		"ATEAPI_ENDPOINT":   "127.0.0.1:9443",
	}
	cfg, err := LoadConfig(func(key string) string { return values[key] })
	if err != nil {
		t.Fatalf("ateapi env: %v", err)
	}
	if cfg.AteapiToken != values["ATEAPI_TOKEN_FILE"] ||
		cfg.AteapiCA != values["ATEAPI_CA_FILE"] ||
		cfg.AteapiEndpoint != values["ATEAPI_ENDPOINT"] {
		t.Fatalf("ateapi config: %+v", cfg)
	}
}

// FEATURE: Installation domain of the actor Git identity (#249).
// SPEC: BROOD_ACTOR_GIT_DOMAIN is optional and has no default; a value must
// be a lowercase DNS name, otherwise the server refuses to start naming the
// setting. Quotes are stripped like every .env value.
func TestConfigActorGitDomain(t *testing.T) {
	cfg, err := LoadConfig(func(string) string { return "" })
	if err != nil || cfg.ActorGitDomain != "" {
		t.Fatalf("unset domain: %q %v", cfg.ActorGitDomain, err)
	}
	cfg, err = LoadConfig(func(k string) string {
		if k == "BROOD_ACTOR_GIT_DOMAIN" {
			return "'brood.corp.example'"
		}
		return ""
	})
	if err != nil || cfg.ActorGitDomain != "brood.corp.example" {
		t.Fatalf("domain: %q %v", cfg.ActorGitDomain, err)
	}
	for _, bad := range []string{"Brood.corp.example", "192.0.2.1", "brood.corp.example.", "brood corp", "https://brood.corp.example", "actor@brood.corp.example"} {
		_, err := LoadConfig(func(k string) string {
			if k == "BROOD_ACTOR_GIT_DOMAIN" {
				return bad
			}
			return ""
		})
		var ce *ConfigError
		if !errors.As(err, &ce) || ce.Setting != "BROOD_ACTOR_GIT_DOMAIN" {
			t.Fatalf("%q accepted or misreported: %v", bad, err)
		}
	}
}
