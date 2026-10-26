package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/do-service/cloud/backend/internal/admin"
	"github.com/do-service/cloud/backend/internal/audit"
	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/config"
	"github.com/do-service/cloud/backend/internal/db"
	"github.com/do-service/cloud/backend/internal/fs"
	"github.com/do-service/cloud/backend/internal/httpserver"
	"github.com/do-service/cloud/backend/internal/jobs"
	"github.com/do-service/cloud/backend/internal/maintenance"
	"github.com/do-service/cloud/backend/internal/metrics"
	"github.com/do-service/cloud/backend/internal/nodes"
	"github.com/do-service/cloud/backend/internal/preview"
	"github.com/do-service/cloud/backend/internal/ratelimit"
	"github.com/do-service/cloud/backend/internal/share"
	"github.com/do-service/cloud/backend/internal/storage/localfs"
	"github.com/do-service/cloud/backend/internal/sync"
	"github.com/do-service/cloud/backend/internal/upload"
	"github.com/do-service/cloud/backend/internal/version"
	"github.com/do-service/cloud/backend/internal/webdav"
	"github.com/do-service/cloud/backend/internal/webui"
	"github.com/do-service/cloud/backend/internal/wopi"
)

// shutdownTimeout is how long in-flight requests get to finish after
// SIGTERM. Long transfers still running past it are cut off; the client is
// expected to resume them.
const shutdownTimeout = 15 * time.Second

// jobWorkers bounds the background jobs one process works at once: enough
// for cleanups and previews to keep up, few enough to leave the database
// pool to requests.
const jobWorkers = 10

func main() {
	// All timestamps are stored and serialized in UTC — pin time.Local so
	// pgx-decoded times and time.Now() don't pick up the host's zone.
	time.Local = time.UTC

	cfg := config.Load()
	// SetDefault also routes the standard log package (goose, net/http's
	// own error log) through this handler, so every line is structured.
	slog.SetDefault(newLogger(os.Stderr, cfg.LogFormat, cfg.LogLevel))

	if err := run(cfg); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func newLogger(w io.Writer, format string, level slog.Level) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == config.LogFormatText {
		return slog.New(slog.NewTextHandler(w, opts))
	}
	return slog.New(slog.NewJSONHandler(w, opts))
}

func run(cfg config.Config) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting", "version", version.Version)

	if err := db.Migrate(cfg.DatabaseURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	pool, err := db.NewPool(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBMinConns, cfg.DBStatementTimeout)
	if err != nil {
		return fmt.Errorf("db pool: %w", err)
	}
	defer pool.Close()
	metrics.RegisterDBPool(pool)
	if err := jobs.Migrate(ctx, pool); err != nil {
		return err
	}

	// Opened up front, before anything uses it, so a fresh install gets its
	// layout without a manual mkdir and a data dir split across
	// filesystems fails the deploy rather than the first upload. /readyz
	// keeps checking it stays writable.
	store, err := localfs.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer store.Close()
	a, err := newApp(ctx, cfg, pool, store)
	if err != nil {
		return err
	}
	handler, queue := a.handler, a.queue

	if err := queue.Start(ctx); err != nil {
		return fmt.Errorf("start jobs: %w", err)
	}
	slog.Info("role", "role", cfg.Role)

	// ReadHeaderTimeout bounds slowloris-style header trickling; IdleTimeout
	// reaps idle keep-alives. No ReadTimeout/WriteTimeout: they cover the
	// whole request, and a multi-GB upload or download over a slow link
	// legitimately takes far longer than any sane global value. Per-route
	// deadlines (http.ResponseController) belong in the handlers that need
	// them.
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
	}
	var servers []*http.Server
	// A worker serves no public HTTP: it only works the queue.
	if cfg.Role != config.RoleWorker {
		servers = append(servers, srv)
	}

	if cfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", metrics.Handler())
		// A worker has no public port, so its liveness probe lives here.
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
		servers = append(servers, &http.Server{
			Addr:              cfg.MetricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
		})
	}

	errCh := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			slog.Info("listening", "addr", s.Addr)
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("listen %s: %w", s.Addr, err)
			}
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		slog.Info("shutting down", "timeout", shutdownTimeout.String())
	case runErr = <-errCh:
	}

	// A fresh context: ctx is already cancelled by the signal.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := queue.Stop(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("stop jobs: %w", err))
	}
	for _, s := range servers {
		if err := s.Shutdown(shutdownCtx); err != nil {
			// Shutdown leaves still-active connections open on timeout;
			// Close cuts them so the process actually exits.
			runErr = errors.Join(runErr, fmt.Errorf("shutdown %s: %w", s.Addr, err), s.Close())
		}
	}
	return runErr
}

// app is the server as run serves it: the public handler, and the job
// queue behind it, not yet started.
type app struct {
	handler http.Handler
	queue   *jobs.Client
}

// newApp assembles the server over pool and store: every service, every
// handler, the queue. Tests build the same (the security matrix), so what
// they check is what runs. Helpers in the background (rate limit sweeps,
// the editor's discovery) stop with ctx.
func newApp(ctx context.Context, cfg config.Config, pool *pgxpool.Pool, store *localfs.Store) (*app, error) {
	files := fs.New(pool, store)
	users := auth.NewUsers(pool)
	if err := bootstrapAdmin(ctx, cfg, users, files); err != nil {
		return nil, fmt.Errorf("bootstrap admin: %w", err)
	}
	// After the first admin, who has no limit: they set the others'.
	users.DefaultQuota = cfg.DefaultQuota
	auditLog := audit.New(pool)
	oidc, err := setUpOIDC(ctx, cfg, pool)
	if err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}
	loginPerIP, loginPerUser := auth.NewLoginLimiters()
	basicFailures := auth.NewBasicFailureLimiter()
	deviceTokenPerIP := auth.NewDeviceTokenLimiter()
	for _, l := range []*ratelimit.Limiter{loginPerIP, loginPerUser, basicFailures, deviceTokenPerIP} {
		go l.Run(ctx, time.Minute)
	}
	appPasswords := auth.NewAppPasswords(pool)
	sessions := auth.NewSessions(pool, cfg.SessionTTL)
	cookies := auth.Cookies{Secure: cfg.CookieSecure()}
	devices := auth.NewDevices(pool)
	withUser := auth.Middleware(sessions, devices, cookies)
	signedIn := func(next http.Handler) http.Handler { return withUser(auth.RequireUser(next)) }
	shares := share.New(pool, files)
	// Without SECRET_KEY the box refuses to seal, and creating a link
	// says why; everything else works.
	box, err := auth.NewSecretBox(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	links := share.NewLinks(shares, box)
	unlockLimiter := ratelimit.New(10*time.Second, 5)
	go unlockLimiter.Run(ctx, time.Minute)
	uploads, err := upload.New(files, cfg.DataDir, signedIn)
	if err != nil {
		return nil, err
	}
	// Stops with ctx, which a shutdown cancels first: long polls answer
	// then, rather than hold up the server's Shutdown.
	hub := sync.NewHub(files)
	go hub.Run(ctx)

	// Set below; the admin API only enqueues at request time.
	var queue *jobs.Client
	var discovery *wopi.Discovery
	if cfg.WOPIDiscoveryURL != "" {
		discovery = wopi.NewDiscovery(cfg.WOPIDiscoveryURL, cfg.WOPIEditorURL)
	}
	deps := httpserver.Deps{
		DB:           pool,
		DataDir:      cfg.DataDir,
		WebUI:        webui.Handler(),
		PublicOrigin: cfg.PublicOrigin(),
		Auth: &auth.Handlers{
			Sessions:     sessions,
			Cookies:      cookies,
			Users:        users,
			LoginPerIP:   loginPerIP,
			LoginPerUser: loginPerUser,
			AppPasswords: appPasswords,
			Devices:      devices,
			OIDC:         oidc,
			// The exchange of a desktop sign-in code, per client IP.
			DeviceTokenPerIP: deviceTokenPerIP,
			OnUserCreated: func(ctx context.Context, u auth.User) error {
				_, err := files.EnsureRoot(ctx, u.ID)
				return err
			},
			Audit: auditLog,
		},
		Nodes: &nodes.Handlers{
			FS: files, Shares: shares, Links: links, Uploads: uploads,
			UnlockLimiter: unlockLimiter, SecureCookies: cfg.CookieSecure(), SignedIn: signedIn, Audit: auditLog,
			Preview: preview.New(files, store, pool),
		},
		Uploads: uploads,
		Sharing: &share.Handlers{Shares: shares, SignedIn: signedIn, Audit: auditLog},
		Sync:    &sync.Handlers{FS: files, Shares: shares, Hub: hub, JournalRetention: cfg.SyncJournalRetention, SignedIn: signedIn},
		Admin: &admin.Handlers{
			Users: users, Audit: auditLog, Devices: devices, SignedIn: signedIn,
			SetUp: func(ctx context.Context, userID string) error {
				_, err := files.EnsureRoot(ctx, userID)
				return err
			},
			Enqueue: func(ctx context.Context, userID string) error {
				_, err := queue.Insert(ctx, admin.DeleteUserArgs{UserID: userID}, nil)
				return err
			},
			System: admin.System{
				PasswordLogin: true,
				Providers: func(context.Context) ([]auth.Provider, error) {
					list := make([]auth.Provider, len(cfg.OIDCProviders))
					for i, p := range cfg.OIDCProviders {
						list[i] = auth.Provider{Slug: p.Slug, Name: p.Name, Issuer: p.Issuer}
					}
					return list, nil
				},
				EditorURL:         cfg.WOPIDiscoveryURL,
				DefaultQuotaBytes: cfg.DefaultQuota,
				AuditRetention:    cfg.AuditRetention,
				TrashRetention:    cfg.TrashRetention,
			},
		},
	}
	deps.TrustedProxies, deps.HSTS = cfg.TrustedProxies, cfg.CookieSecure()
	if discovery != nil {
		deps.EditorOrigin = discovery.Origin
		go discovery.Warm(ctx)
		deps.WOPI = &wopi.Handlers{
			FS: files, Tokens: wopi.NewTokens(pool), Discovery: discovery,
			HostURL: cfg.WOPIHostURL, PublicOrigin: cfg.PublicBaseURL, SignedIn: signedIn,
		}
		deps.Admin.System.Editor = func(ctx context.Context) (bool, error) {
			_, err := discovery.Editor(ctx)
			return true, err
		}
	}
	dav := webdav.New(files, shares, auditLog)
	basic := auth.BasicAuth(appPasswords, basicFailures)
	deps.WebDAV = func(prefix string) http.Handler { return basic(dav(prefix)) }
	queue, err = jobs.New(pool, jobs.Options{
		Work: cfg.Role != config.RoleAPI,
		Register: func(w *river.Workers) []*river.PeriodicJob {
			river.AddWorker(w, &upload.CleanupWorker{FS: files, DataDir: cfg.DataDir})
			river.AddWorker(w, &admin.DeleteUserWorker{FS: files, Users: users})
			river.AddWorker(w, &nodes.CopyWorker{FS: files})
			versions := fs.VersionPolicy{MaxCount: cfg.VersionsMaxCount, MaxAge: cfg.VersionsMaxAge}
			periodic := append(maintenance.Setup(w, files, cfg.TrashRetention, versions), upload.PeriodicCleanup())
			periodic = append(periodic, maintenance.SetupJournal(w, files, cfg.SyncJournalRetention)...)
			return append(periodic, maintenance.SetupAudit(w, auditLog, cfg.AuditRetention)...)
		},
		MaxWorkers: jobWorkers,
	})
	if err != nil {
		return nil, err
	}
	deps.Nodes.Jobs = nodes.RiverCopyJobs{Client: queue.Client}
	return &app{handler: httpserver.New(deps), queue: queue}, nil
}

// setUpOIDC writes the env-configured providers to the database and
// returns the enabled ones, or nil when there are none.
func setUpOIDC(ctx context.Context, cfg config.Config, pool *pgxpool.Pool) (*auth.OIDC, error) {
	box, err := auth.NewSecretBox(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	providers := auth.NewProviders(pool, box)
	if err := providers.Sync(ctx, cfg.OIDCProviders); err != nil {
		return nil, err
	}
	enabled, err := providers.Enabled(ctx)
	if err != nil || len(enabled) == 0 {
		return nil, err
	}
	for _, p := range enabled {
		slog.Info("OIDC provider enabled", "provider", p.Slug, "issuer", p.Issuer)
	}
	return auth.NewOIDC(cfg.PublicBaseURL, enabled), nil
}

// bootstrapAdmin creates the first administrator from ADMIN_USERNAME and
// ADMIN_PASSWORD on a fresh install, so the first login needs no manual
// step (G4). Once any admin exists it does nothing, whatever the env says:
// changing the variables later must not reset or add accounts.
func bootstrapAdmin(ctx context.Context, cfg config.Config, users *auth.Users, files *fs.Service) error {
	has, err := users.HasAdmin(ctx)
	if err != nil || has {
		return err
	}
	if cfg.AdminUsername == "" {
		slog.Warn("no administrator exists yet: set ADMIN_USERNAME and ADMIN_PASSWORD and restart to create one")
		return nil
	}
	u, err := users.CreateLocal(ctx, cfg.AdminUsername, "", cfg.AdminPassword, true)
	if err != nil {
		return err
	}
	if _, err := files.EnsureRoot(ctx, u.ID); err != nil {
		return fmt.Errorf("create root of %s: %w", u.Username, err)
	}
	slog.Info("created the first administrator; change the password after signing in", "username", u.Username)
	return nil
}
