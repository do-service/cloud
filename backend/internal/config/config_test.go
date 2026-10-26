package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := env[key]
		return v, ok
	}
}

func TestParse_Defaults(t *testing.T) {
	cfg, err := Parse(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Config{
		HTTPAddr:           ":8080",
		MetricsAddr:        ":9090",
		DatabaseURL:        "postgres://cloud:cloud@localhost:55433/cloud?sslmode=disable",
		DBMaxConns:         20,
		DBMinConns:         2,
		DBStatementTimeout: 30 * time.Second,
		PublicBaseURL:      "http://localhost:5173",
		DataDir:            "./data",
		SessionTTL:         720 * time.Hour,
		LogLevel:           slog.LevelInfo,
		LogFormat:          LogFormatJSON,
		Role:               RoleAll,
		TrashRetention:     30 * 24 * time.Hour,
		VersionsMaxCount:   50,
		WOPIHostURL:        "http://localhost:5173",
		AuditRetention:     365 * 24 * time.Hour,
	}
	want.SyncJournalRetention = 30 * 24 * time.Hour
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestParse_Overrides(t *testing.T) {
	tenGiB := int64(10 << 30)
	cfg, err := Parse(lookupFrom(map[string]string{
		"HTTP_ADDR":            "127.0.0.1:8000",
		"METRICS_ADDR":         "127.0.0.1:9100",
		"DATABASE_URL":         "postgres://u:p@db:5432/cloud",
		"DB_MAX_CONNS":         "50",
		"DB_MIN_CONNS":         "0",
		"DB_STATEMENT_TIMEOUT": "0s",
		"PUBLIC_BASE_URL":      "https://cloud.example.com/",
		"DATA_DIR":             "/var/lib/cloud",
		"SESSION_TTL":          "24h",
		"LOG_LEVEL":            "DEBUG",
		"LOG_FORMAT":           "Text",
		"ROLE":                 "Worker",
		"TRASH_RETENTION":      "168h",
		"VERSIONS_MAX_COUNT":   "0",
		"VERSIONS_MAX_AGE":     "720h",
		"WOPI_DISCOVERY_URL":   "http://collabora:9980/hosting/discovery",
		"DEFAULT_QUOTA":        "10GiB",
		"AUDIT_RETENTION":      "0s",
		"WOPI_HOST_URL":        "http://cloud:8080/",
		"WOPI_EDITOR_URL":      "http://localhost:9981/",
	}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Config{
		HTTPAddr:           "127.0.0.1:8000",
		MetricsAddr:        "127.0.0.1:9100",
		DatabaseURL:        "postgres://u:p@db:5432/cloud",
		DBMaxConns:         50,
		DBMinConns:         0,
		DBStatementTimeout: 0,
		PublicBaseURL:      "https://cloud.example.com",
		DataDir:            "/var/lib/cloud",
		SessionTTL:         24 * time.Hour,
		LogLevel:           slog.LevelDebug,
		LogFormat:          LogFormatText,
		Role:               RoleWorker,
		TrashRetention:     7 * 24 * time.Hour,
		VersionsMaxAge:     30 * 24 * time.Hour,
		WOPIDiscoveryURL:   "http://collabora:9980/hosting/discovery",
		WOPIHostURL:        "http://cloud:8080",
		WOPIEditorURL:      "http://localhost:9981",
		DefaultQuota:       &tenGiB,
	}
	want.SyncJournalRetention = 30 * 24 * time.Hour
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("cfg = %+v, want %+v", cfg, want)
	}
}

func TestParse_EmptyMetricsAddrDisables(t *testing.T) {
	cfg, err := Parse(lookupFrom(map[string]string{"METRICS_ADDR": ""}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.MetricsAddr != "" {
		t.Fatalf("MetricsAddr = %q, want empty", cfg.MetricsAddr)
	}
}

// Blank values of required settings mean "use the default", not "empty".
func TestParse_BlankFallsBackToDefault(t *testing.T) {
	cfg, err := Parse(lookupFrom(map[string]string{"HTTP_ADDR": "  ", "DATA_DIR": ""}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.DataDir != "./data" {
		t.Fatalf("HTTPAddr %q, DataDir %q", cfg.HTTPAddr, cfg.DataDir)
	}
}

func TestParse_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"max conns not int", map[string]string{"DB_MAX_CONNS": "many"}, "DB_MAX_CONNS must be an integer"},
		{"max conns zero", map[string]string{"DB_MAX_CONNS": "0"}, "DB_MAX_CONNS must be at least 1"},
		{"max conns overflow", map[string]string{"DB_MAX_CONNS": "99999999999"}, "DB_MAX_CONNS must be an integer"},
		{"min conns not int", map[string]string{"DB_MIN_CONNS": "1.5"}, "DB_MIN_CONNS must be an integer"},
		{"min conns negative", map[string]string{"DB_MIN_CONNS": "-1"}, "DB_MIN_CONNS must be between"},
		{"min above max", map[string]string{"DB_MAX_CONNS": "5", "DB_MIN_CONNS": "6"}, "DB_MIN_CONNS must be between"},
		{"statement timeout malformed", map[string]string{"DB_STATEMENT_TIMEOUT": "30"}, "DB_STATEMENT_TIMEOUT must be a duration"},
		{"statement timeout negative", map[string]string{"DB_STATEMENT_TIMEOUT": "-1s"}, "DB_STATEMENT_TIMEOUT must not be negative"},
		{"base url relative", map[string]string{"PUBLIC_BASE_URL": "/cloud"}, "PUBLIC_BASE_URL must be an absolute"},
		{"base url no host", map[string]string{"PUBLIC_BASE_URL": "https://"}, "PUBLIC_BASE_URL must be an absolute"},
		{"base url bad scheme", map[string]string{"PUBLIC_BASE_URL": "ftp://cloud.example.com"}, "PUBLIC_BASE_URL must be an absolute"},
		{"editor url relative", map[string]string{"WOPI_EDITOR_URL": "localhost:9981"}, "WOPI_EDITOR_URL must be an absolute"},
		{"editor url with path", map[string]string{"WOPI_EDITOR_URL": "https://office.example.com/docs"}, "WOPI_EDITOR_URL must be an origin"},
		{"log level", map[string]string{"LOG_LEVEL": "verbose"}, "LOG_LEVEL must be one of"},
		{"log format", map[string]string{"LOG_FORMAT": "logfmt"}, "LOG_FORMAT must be"},
		{"role", map[string]string{"ROLE": "scheduler"}, "ROLE must be"},
		{"negative retention", map[string]string{"TRASH_RETENTION": "-1h"}, "must not be negative"},
		{"negative journal retention", map[string]string{"SYNC_JOURNAL_RETENTION": "-1h"}, "SYNC_JOURNAL_RETENTION must not be negative"},
		{"metrics on http addr", map[string]string{"HTTP_ADDR": ":8080", "METRICS_ADDR": ":8080"}, "METRICS_ADDR must differ"},
		{"session ttl too short", map[string]string{"SESSION_TTL": "10m"}, "SESSION_TTL must be at least 1h"},
		{"admin without password", map[string]string{"ADMIN_USERNAME": "admin"}, "must be set together"},
		{"secret key not base64", map[string]string{"SECRET_KEY": "not base64!"}, "SECRET_KEY must be 32 random bytes"},
		{"secret key short", map[string]string{"SECRET_KEY": "c2hvcnQ="}, "SECRET_KEY must be 32 random bytes"},
		{"oidc bad slug", map[string]string{"OIDC_PROVIDERS": "Key_Cloak"}, "slug \"Key_Cloak\" must be"},
		{"oidc slug twice", oidcEnv(map[string]string{"OIDC_PROVIDERS": "kc,kc"}), "listed twice"},
		{"oidc no issuer", map[string]string{"OIDC_PROVIDERS": "kc", "OIDC_KC_CLIENT_ID": "cloud"}, "OIDC_KC_ISSUER must be an absolute"},
		{"oidc no client id", map[string]string{"OIDC_PROVIDERS": "kc", "OIDC_KC_ISSUER": "https://id.example.com"}, "OIDC_KC_CLIENT_ID must be set"},
		{"oidc scopes without openid", oidcEnv(map[string]string{"OIDC_KC_SCOPES": "profile email"}), "OIDC_KC_SCOPES must include openid"},
		{"oidc bad bool", oidcEnv(map[string]string{"OIDC_KC_AUTO_CREATE": "maybe"}), "OIDC_KC_AUTO_CREATE must be true or false"},
		{"oidc secret without key", oidcEnv(map[string]string{"OIDC_KC_CLIENT_SECRET": "s3cret"}), "SECRET_KEY must be set"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(lookupFrom(tc.env))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestParse_ReportsAllErrors(t *testing.T) {
	_, err := Parse(lookupFrom(map[string]string{"LOG_LEVEL": "verbose", "LOG_FORMAT": "xml"}))
	if err == nil || !strings.Contains(err.Error(), "LOG_LEVEL") || !strings.Contains(err.Error(), "LOG_FORMAT") {
		t.Fatalf("err = %v, want both LOG_LEVEL and LOG_FORMAT reported", err)
	}
}

func TestPublicOriginAndCookieSecure(t *testing.T) {
	cases := []struct {
		base, origin string
		secure       bool
	}{
		{"http://localhost:5173", "http://localhost:5173", false},
		{"https://Cloud.Example.com/files", "https://cloud.example.com", true},
	}
	for _, tc := range cases {
		cfg := Config{PublicBaseURL: tc.base}
		if got := cfg.PublicOrigin(); got != tc.origin {
			t.Errorf("%s: origin %q, want %q", tc.base, got, tc.origin)
		}
		if got := cfg.CookieSecure(); got != tc.secure {
			t.Errorf("%s: secure %v", tc.base, got)
		}
	}
}

// oidcEnv is a valid one-provider configuration ("kc") with overrides.
func oidcEnv(over map[string]string) map[string]string {
	env := map[string]string{
		"OIDC_PROVIDERS":    "kc",
		"OIDC_KC_ISSUER":    "https://id.example.com/realms/main",
		"OIDC_KC_CLIENT_ID": "cloud",
	}
	for k, v := range over {
		env[k] = v
	}
	return env
}

func TestParse_OIDCProviders(t *testing.T) {
	key := strings.Repeat("k", SecretKeyLen)
	cfg, err := Parse(lookupFrom(map[string]string{
		"SECRET_KEY":                 "a2tra2tra2tra2tra2tra2tra2tra2tra2tra2tra2s=",
		"OIDC_PROVIDERS":             " kc , git-lab ",
		"OIDC_KC_ISSUER":             "https://id.example.com/realms/main/",
		"OIDC_KC_CLIENT_ID":          "cloud",
		"OIDC_KC_CLIENT_SECRET":      "s3cret",
		"OIDC_GIT_LAB_NAME":          "GitLab",
		"OIDC_GIT_LAB_ISSUER":        "http://gitlab.internal",
		"OIDC_GIT_LAB_CLIENT_ID":     "abc",
		"OIDC_GIT_LAB_SCOPES":        "openid,email",
		"OIDC_GIT_LAB_AUTO_CREATE":   "false",
		"OIDC_GIT_LAB_LINK_BY_EMAIL": "true",
	}))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if string(cfg.SecretKey) != key {
		t.Fatalf("SecretKey = %q", cfg.SecretKey)
	}
	want := []OIDCProvider{
		{Slug: "kc", Name: "kc", Issuer: "https://id.example.com/realms/main", ClientID: "cloud", ClientSecret: "s3cret",
			Scopes: []string{"openid", "profile", "email"}, AutoCreate: true},
		{Slug: "git-lab", Name: "GitLab", Issuer: "http://gitlab.internal", ClientID: "abc",
			Scopes: []string{"openid", "email"}, LinkByEmail: true},
	}
	if !reflect.DeepEqual(cfg.OIDCProviders, want) {
		t.Fatalf("providers = %+v\nwant %+v", cfg.OIDCProviders, want)
	}
}

func TestParse_Size(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "1048576": 1 << 20, "500MB": 500 << 20, "2 GiB": 2 << 30, "1TB": 1 << 40} {
		cfg, err := Parse(lookupFrom(map[string]string{"DEFAULT_QUOTA": in}))
		if err != nil || cfg.DefaultQuota == nil || *cfg.DefaultQuota != want {
			t.Errorf("DEFAULT_QUOTA=%q: %v, %v", in, cfg.DefaultQuota, err)
		}
	}
	for _, bad := range []string{"10XB", "-1", "1.5GB", "99999999999TiB"} {
		if _, err := Parse(lookupFrom(map[string]string{"DEFAULT_QUOTA": bad})); err == nil {
			t.Errorf("DEFAULT_QUOTA=%q accepted", bad)
		}
	}
}

func TestParse_TrustedProxies(t *testing.T) {
	cfg, err := Parse(lookupFrom(map[string]string{"TRUSTED_PROXIES": " 10.0.0.0/8, 192.168.1.5,::1 ,172.16.3.4/12"}))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range cfg.TrustedProxies {
		got = append(got, p.String())
	}
	if strings.Join(got, ",") != "10.0.0.0/8,192.168.1.5/32,::1/128,172.16.0.0/12" {
		t.Fatalf("TrustedProxies = %v", got)
	}
	for _, bad := range []string{"10.0.0.0/33", "proxy.local", "10.0.0"} {
		if _, err := Parse(lookupFrom(map[string]string{"TRUSTED_PROXIES": bad})); err == nil {
			t.Errorf("TRUSTED_PROXIES=%q accepted", bad)
		}
	}
}
