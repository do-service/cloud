// Package config reads the server configuration from the environment.
// There is deliberately no config file: every setting is an env var with a
// dev-friendly default, and an invalid value fails startup rather than
// falling back silently.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddr string
	// MetricsAddr is a separate listener for /metrics so it never ends up
	// behind the public reverse proxy. Empty disables it.
	MetricsAddr string

	DatabaseURL string
	// Pool sizing for a self-hosted instance with modest concurrency;
	// pgxpool's own default (max(4, numCPU)) is too small once uploads,
	// WebDAV clients and the UI contend for connections.
	DBMaxConns int32
	DBMinConns int32
	// Server-side cap on any single statement issued through the pool (see
	// db.NewPool); 0 disables it.
	DBStatementTimeout time.Duration

	// PublicBaseURL is the origin the browser uses to reach the app (share
	// links, redirects, cookie Secure flag) — not the bind address.
	PublicBaseURL string
	// DataDir is the root of on-disk file storage.
	DataDir string

	// SessionTTL is how long a session lives after its last use.
	SessionTTL time.Duration
	// AdminUsername and AdminPassword create the first administrator on a
	// fresh install; ignored once any admin exists.
	AdminUsername string
	AdminPassword string

	// SecretKey (SECRET_KEY, 32 bytes base64) encrypts secrets kept in the
	// database, such as OIDC client secrets. Losing it means re-entering
	// them; leaking it together with a dump exposes them.
	SecretKey []byte
	// OIDCProviders come from OIDC_PROVIDERS and OIDC_<SLUG>_*; they are
	// written to auth_providers on every start (see auth.SyncProviders).
	OIDCProviders []OIDCProvider

	LogLevel slog.Level
	// LogFormat is "json" (production, machine-parsed) or "text" (dev).
	LogFormat string
	// TrashRetention is how long deleted items stay in the trash before
	// being purged (R8); 0 keeps them until the user empties the trash.
	TrashRetention time.Duration
	// VersionsMaxCount is how many earlier versions a file keeps, newest
	// first (R7); 0 is no limit.
	VersionsMaxCount int
	// VersionsMaxAge drops versions older than this; 0 is no limit.
	VersionsMaxAge time.Duration
	// TrustedProxies are the reverse proxies whose X-Forwarded-For names
	// the client (TRUSTED_PROXIES: CIDRs or addresses, comma-separated).
	// Empty trusts none: the peer address is the client.
	TrustedProxies []netip.Prefix
	// DefaultQuota is the quota of new accounts in bytes (R16); nil is no
	// limit. Admins change it per user.
	DefaultQuota *int64
	// AuditRetention is how long audit log entries are kept; 0 is forever.
	AuditRetention time.Duration
	// SyncJournalRetention is how long the sync change journal keeps rows,
	// so how long a sync client may stay offline before it has to resync
	// in full; 0 is forever.
	SyncJournalRetention time.Duration
	// WOPIDiscoveryURL is the online editor's discovery document
	// (Collabora: http://collabora:9980/hosting/discovery); empty leaves
	// editing in the browser off (R12).
	WOPIDiscoveryURL string
	// WOPIHostURL is this server's origin as the editor's server reaches
	// it, which can differ from the browser's (inside docker:
	// http://cloud:8080). PUBLIC_BASE_URL when unset.
	WOPIHostURL string
	// WOPIEditorURL is the editor's origin as the browser reaches it, when
	// the server fetches the discovery at another address (inside docker:
	// http://onlyoffice for the server, http://localhost:9981 for the
	// browser). Empty: the URLs in the discovery as they are.
	WOPIEditorURL string
	// Role is what this process does (architecture §2): "all" by default,
	// "api" to serve HTTP and only enqueue background jobs, "worker" to run
	// them without serving the public HTTP port.
	Role string
}

const (
	LogFormatJSON = "json"
	LogFormatText = "text"

	RoleAll    = "all"
	RoleAPI    = "api"
	RoleWorker = "worker"
)

// OIDCProvider is one sign-in provider as configured through the env.
type OIDCProvider struct {
	// Slug names the provider in URLs (/api/v1/auth/oidc/{slug}/…) and
	// in its env variables; renaming it orphans the users' identities.
	Slug   string
	Name   string
	Issuer string
	// ClientSecret is empty for a public client, which relies on PKCE
	// alone.
	ClientID     string
	ClientSecret string
	Scopes       []string
	// AutoCreate makes an account for a first-time user; otherwise only
	// users with an existing identity (or a linked email) get in.
	AutoCreate bool
	// LinkByEmail attaches a first-time sign-in to the existing account
	// with the same email, if the provider says the email is verified.
	// Off by default: a provider that lets users set an unverified email
	// and still claims it verified would hand out other people's accounts.
	LinkByEmail bool
}

// validSlug keeps provider slugs safe in URL paths and env variable names.
var validSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// SecretKeyLen is an AES-256 key.
const SecretKeyLen = 32

// Load is Parse over the process environment. An invalid configuration
// exits the process: running with a half-understood config is worse than
// not starting.
func Load() Config {
	cfg, err := Parse(os.LookupEnv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	return cfg
}

// Parse builds a Config from lookup (os.LookupEnv in production, a map in
// tests), returning every invalid setting at once so a misconfigured
// deployment doesn't need one restart per typo.
func Parse(lookup func(string) (string, bool)) (Config, error) {
	p := parser{lookup: lookup}
	cfg := Config{
		HTTPAddr:           p.str("HTTP_ADDR", ":8080"),
		MetricsAddr:        p.optional("METRICS_ADDR", ":9090"),
		DatabaseURL:        p.str("DATABASE_URL", "postgres://cloud:cloud@localhost:55433/cloud?sslmode=disable"),
		DBMaxConns:         p.int32("DB_MAX_CONNS", 20),
		DBMinConns:         p.int32("DB_MIN_CONNS", 2),
		DBStatementTimeout: p.duration("DB_STATEMENT_TIMEOUT", 30*time.Second),
		PublicBaseURL:      strings.TrimRight(p.str("PUBLIC_BASE_URL", "http://localhost:5173"), "/"),
		DataDir:            p.str("DATA_DIR", "./data"),
		SessionTTL:         p.duration("SESSION_TTL", 30*24*time.Hour),
		AdminUsername:      p.str("ADMIN_USERNAME", ""),
		AdminPassword:      p.str("ADMIN_PASSWORD", ""),
		SecretKey:          p.secretKey("SECRET_KEY"),
		OIDCProviders:      p.oidcProviders(),
		LogLevel:           p.level("LOG_LEVEL", slog.LevelInfo),
		LogFormat:          strings.ToLower(p.str("LOG_FORMAT", LogFormatJSON)),
		Role:               strings.ToLower(p.str("ROLE", RoleAll)),
		TrashRetention:     p.duration("TRASH_RETENTION", 30*24*time.Hour),
		VersionsMaxCount:   int(p.int32("VERSIONS_MAX_COUNT", 50)),
		VersionsMaxAge:     p.duration("VERSIONS_MAX_AGE", 0),
		TrustedProxies:     p.prefixes("TRUSTED_PROXIES"),
		DefaultQuota:       p.size("DEFAULT_QUOTA"),
		AuditRetention:     p.duration("AUDIT_RETENTION", 365*24*time.Hour),
		WOPIDiscoveryURL:   p.str("WOPI_DISCOVERY_URL", ""),
		WOPIHostURL:        strings.TrimRight(p.str("WOPI_HOST_URL", ""), "/"),
		WOPIEditorURL:      strings.TrimRight(p.str("WOPI_EDITOR_URL", ""), "/"),
	}
	cfg.SyncJournalRetention = p.duration("SYNC_JOURNAL_RETENTION", 30*24*time.Hour)
	if cfg.WOPIHostURL == "" {
		cfg.WOPIHostURL = cfg.PublicBaseURL
	}
	for name, v := range map[string]string{"WOPI_DISCOVERY_URL": cfg.WOPIDiscoveryURL, "WOPI_HOST_URL": cfg.WOPIHostURL, "WOPI_EDITOR_URL": cfg.WOPIEditorURL} {
		if u, err := url.Parse(v); v != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
			p.fail("%s must be an absolute http(s) URL, got %q", name, v)
		}
	}
	// Only the origin is replaced in the editor's URLs: a path would be
	// silently dropped.
	if u, err := url.Parse(cfg.WOPIEditorURL); err == nil && (u.Path != "" || u.RawQuery != "") {
		p.fail("WOPI_EDITOR_URL must be an origin (scheme://host[:port]), got %q", cfg.WOPIEditorURL)
	}

	if cfg.DBMaxConns < 1 {
		p.fail("DB_MAX_CONNS must be at least 1, got %d", cfg.DBMaxConns)
	}
	if cfg.DBMinConns < 0 || cfg.DBMinConns > cfg.DBMaxConns {
		p.fail("DB_MIN_CONNS must be between 0 and DB_MAX_CONNS (%d), got %d", cfg.DBMaxConns, cfg.DBMinConns)
	}
	if cfg.DBStatementTimeout < 0 {
		p.fail("DB_STATEMENT_TIMEOUT must not be negative, got %s", cfg.DBStatementTimeout)
	}
	if u, err := url.Parse(cfg.PublicBaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		p.fail("PUBLIC_BASE_URL must be an absolute http(s) URL, got %q", cfg.PublicBaseURL)
	}
	if (cfg.AdminUsername == "") != (cfg.AdminPassword == "") {
		p.fail("ADMIN_USERNAME and ADMIN_PASSWORD must be set together")
	}
	for _, op := range cfg.OIDCProviders {
		if op.ClientSecret != "" && cfg.SecretKey == nil {
			p.fail("SECRET_KEY must be set to store the client secret of OIDC provider %q", op.Slug)
			break
		}
	}
	if cfg.AuditRetention < 0 {
		p.fail("AUDIT_RETENTION must not be negative, got %s", cfg.AuditRetention)
	}
	if cfg.SyncJournalRetention < 0 {
		p.fail("SYNC_JOURNAL_RETENTION must not be negative, got %s", cfg.SyncJournalRetention)
	}
	if cfg.SessionTTL < time.Hour {
		p.fail("SESSION_TTL must be at least 1h, got %s", cfg.SessionTTL)
	}
	if cfg.LogFormat != LogFormatJSON && cfg.LogFormat != LogFormatText {
		p.fail("LOG_FORMAT must be %q or %q, got %q", LogFormatJSON, LogFormatText, cfg.LogFormat)
	}
	if cfg.TrashRetention < 0 || cfg.VersionsMaxAge < 0 || cfg.VersionsMaxCount < 0 {
		p.fail("TRASH_RETENTION, VERSIONS_MAX_AGE and VERSIONS_MAX_COUNT must not be negative")
	}
	if cfg.Role != RoleAll && cfg.Role != RoleAPI && cfg.Role != RoleWorker {
		p.fail("ROLE must be %q, %q or %q, got %q", RoleAll, RoleAPI, RoleWorker, cfg.Role)
	}
	if cfg.MetricsAddr != "" && cfg.MetricsAddr == cfg.HTTPAddr {
		p.fail("METRICS_ADDR must differ from HTTP_ADDR (%q)", cfg.HTTPAddr)
	}

	if err := errors.Join(p.errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// PublicOrigin is PUBLIC_BASE_URL reduced to scheme://host[:port], the
// form a browser's Origin header takes.
func (c Config) PublicOrigin() string {
	u, err := url.Parse(c.PublicBaseURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

// CookieSecure reports whether browsers reach the app over https, which
// decides the session cookie's Secure flag and name.
func (c Config) CookieSecure() bool {
	return strings.HasPrefix(strings.ToLower(c.PublicBaseURL), "https://")
}

type parser struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (p *parser) get(key string) string {
	v, _ := p.lookup(key)
	return strings.TrimSpace(v)
}

func (p *parser) fail(format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf(format, args...))
}

// str returns the variable, or fallback when it is unset or blank.
func (p *parser) str(key, fallback string) string {
	if v := p.get(key); v != "" {
		return v
	}
	return fallback
}

// optional is like str, but a variable that is set to an empty value stays
// empty — that is how an optional listener is switched off.
func (p *parser) optional(key, fallback string) string {
	v, ok := p.lookup(key)
	if !ok {
		return fallback
	}
	return strings.TrimSpace(v)
}

func (p *parser) int32(key string, fallback int32) int32 {
	v := p.get(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		p.fail("%s must be an integer, got %q", key, v)
		return fallback
	}
	return int32(n)
}

func (p *parser) duration(key string, fallback time.Duration) time.Duration {
	v := p.get(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		p.fail("%s must be a duration like \"30s\", got %q", key, v)
		return fallback
	}
	return d
}

// prefixes reads a comma-separated list of CIDRs; a bare address is the
// network of just that address.
func (p *parser) prefixes(key string) []netip.Prefix {
	var out []netip.Prefix
	for part := range strings.SplitSeq(p.get(key), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !strings.Contains(part, "/") {
			addr, err := netip.ParseAddr(part)
			if err != nil {
				p.fail("%s: %q is not an address or CIDR", key, part)
				continue
			}
			out = append(out, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
			continue
		}
		prefix, err := netip.ParsePrefix(part)
		if err != nil {
			p.fail("%s: %q is not an address or CIDR", key, part)
			continue
		}
		out = append(out, prefix.Masked())
	}
	return out
}

// sizeUnits are the suffixes size accepts, binary as storage counts.
var sizeUnits = []struct {
	suffix string
	bytes  int64
}{{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}}

// size reads a byte count like "10GiB", "500MB" (binary units either way)
// or plain bytes; empty is nil.
func (p *parser) size(key string) *int64 {
	v := strings.TrimSpace(p.get(key))
	if v == "" {
		return nil
	}
	num, unit := v, int64(1)
	for _, u := range sizeUnits {
		if n, ok := strings.CutSuffix(v, u.suffix); ok {
			num, unit = strings.TrimSpace(n), u.bytes
			break
		}
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 0 || n > (1<<62)/unit {
		p.fail("%s must be a size like \"10GiB\" or a number of bytes, got %q", key, v)
		return nil
	}
	n *= unit
	return &n
}

func (p *parser) boolean(key string, fallback bool) bool {
	v := p.get(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		p.fail("%s must be true or false, got %q", key, v)
		return fallback
	}
	return b
}

func (p *parser) secretKey(key string) []byte {
	v := p.get(key)
	if v == "" {
		return nil
	}
	k, err := base64.StdEncoding.DecodeString(v)
	if err != nil || len(k) != SecretKeyLen {
		p.fail("%s must be %d random bytes in base64 (openssl rand -base64 %d)", key, SecretKeyLen, SecretKeyLen)
		return nil
	}
	return k
}

// oidcProviders reads OIDC_PROVIDERS, a comma-separated list of slugs, and
// for each slug the variables OIDC_<SLUG>_* (dashes become underscores).
func (p *parser) oidcProviders() []OIDCProvider {
	var out []OIDCProvider
	seen := map[string]bool{}
	for _, slug := range strings.Split(p.get("OIDC_PROVIDERS"), ",") {
		slug = strings.TrimSpace(slug)
		if slug == "" {
			continue
		}
		if !validSlug.MatchString(slug) {
			p.fail("OIDC_PROVIDERS: slug %q must be lowercase letters, digits and dashes, up to 32", slug)
			continue
		}
		if seen[slug] {
			p.fail("OIDC_PROVIDERS: slug %q listed twice", slug)
			continue
		}
		seen[slug] = true
		prefix := "OIDC_" + strings.ToUpper(strings.ReplaceAll(slug, "-", "_")) + "_"
		op := OIDCProvider{
			Slug:         slug,
			Name:         p.str(prefix+"NAME", slug),
			Issuer:       strings.TrimRight(p.get(prefix+"ISSUER"), "/"),
			ClientID:     p.get(prefix + "CLIENT_ID"),
			ClientSecret: p.get(prefix + "CLIENT_SECRET"),
			Scopes:       strings.Fields(strings.ReplaceAll(p.str(prefix+"SCOPES", "openid profile email"), ",", " ")),
			AutoCreate:   p.boolean(prefix+"AUTO_CREATE", true),
			LinkByEmail:  p.boolean(prefix+"LINK_BY_EMAIL", false),
		}
		// http is allowed for an IdP on the same private network (and
		// dev); the browser leg is protected by state and PKCE either way.
		if u, err := url.Parse(op.Issuer); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			p.fail("%sISSUER must be an absolute http(s) URL, got %q", prefix, op.Issuer)
		}
		if op.ClientID == "" {
			p.fail("%sCLIENT_ID must be set", prefix)
		}
		hasOpenID := false
		for _, s := range op.Scopes {
			hasOpenID = hasOpenID || s == "openid"
		}
		if !hasOpenID {
			p.fail("%sSCOPES must include openid, got %q", prefix, strings.Join(op.Scopes, " "))
		}
		out = append(out, op)
	}
	return out
}

func (p *parser) level(key string, fallback slog.Level) slog.Level {
	v := p.get(key)
	if v == "" {
		return fallback
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		p.fail("%s must be one of debug, info, warn, error, got %q", key, v)
		return fallback
	}
	return l
}
