// Command benchgen fills a database with a synthetic file tree for the
// benchmarks in bench/ (docs/mvp-tasks.md E1-8): users with realistic
// trees, a folder of exactly 50 000 items, and a signed-in session per
// user whose token goes to a file the k6 scripts read.
//
// Only metadata is generated: blobs point at the s3 backend with a key
// that doesn't exist, so listing works and downloads don't — nothing
// is written to disk besides the database.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/do-service/cloud/backend/internal/db"
)

type options struct {
	databaseURL   string
	users         int
	nodes         int
	bigDirs       int
	bigDirSize    int
	maxDepth      int
	sampleDirs    int
	seed          uint64
	workers       int
	tokensPath    string
	allowExisting bool
}

func main() {
	time.Local = time.UTC
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	var o options
	flag.StringVar(&o.databaseURL, "database-url", os.Getenv("BENCH_DATABASE_URL"), "database to fill (default $BENCH_DATABASE_URL)")
	flag.IntVar(&o.users, "users", 2000, "number of users")
	flag.IntVar(&o.nodes, "nodes", 20_000_000, "total nodes across all trees, roots included")
	flag.IntVar(&o.bigDirs, "big-dirs", 1, "how many users get a folder with exactly -big-dir-size items")
	flag.IntVar(&o.bigDirSize, "big-dir-size", 50_000, "items in each big folder")
	flag.IntVar(&o.maxDepth, "max-depth", 9, "deepest folder level below a root; files go one deeper")
	flag.IntVar(&o.sampleDirs, "sample-dirs", 20, "folder ids per user written to the tokens file for random listing")
	flag.Uint64Var(&o.seed, "seed", 1, "random seed: the same seed and sizes give the same rows")
	flag.IntVar(&o.workers, "workers", max(runtime.NumCPU()/2, 1), "trees generated and copied in parallel")
	flag.StringVar(&o.tokensPath, "tokens", "tokens.jsonl", "where to write the users' session tokens, one JSON object per line")
	flag.BoolVar(&o.allowExisting, "allow-existing", false, "add to a database that already has users (never point this at real data)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, o); err != nil {
		slog.Error("benchgen failed", "err", err)
		os.Exit(1)
	}
}

// tokenLine is one line of the tokens file, the k6 scripts' view of a user.
type tokenLine struct {
	UserID   string `json:"userId"`
	Username string `json:"username"`
	Token    string `json:"token"`
	RootID   string `json:"rootId"`
	BigDirID string `json:"bigDirId,omitempty"`
	// Dirs are folders to list at random, the root first.
	Dirs []string `json:"dirs"`
}

type userResult struct {
	id    uuid
	name  string
	token string
	used  int64
	line  tokenLine
}

func run(ctx context.Context, o options) error {
	start := time.Now()
	if o.databaseURL == "" {
		return errors.New("set -database-url or BENCH_DATABASE_URL")
	}
	if o.users < 1 || o.workers < 1 || o.maxDepth < 1 || o.bigDirs < 0 || o.bigDirSize < 1 {
		return errors.New("-users, -workers, -max-depth and -big-dir-size must be positive, -big-dirs not negative")
	}
	sizes, err := splitNodes(rand.New(rand.NewPCG(o.seed, ^uint64(0))), o.users, o.nodes, o.bigDirs, o.bigDirSize)
	if err != nil {
		return err
	}

	if err := db.Migrate(o.databaseURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	pool, err := openPool(ctx, o.databaseURL, int32(o.workers+2))
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := checkNoPending(ctx, pool); err != nil {
		return err
	}
	var existing int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 && !o.allowExisting {
		return fmt.Errorf("database already has %d users; benchgen only fills an empty database unless -allow-existing is given", existing)
	}

	objs, err := dropForLoad(ctx, pool)
	if err != nil {
		return fmt.Errorf("drop indexes: %w", err)
	}
	slog.Info("indexes and constraints dropped for the load", "count", len(objs))

	results := make([]userResult, o.users)
	if err := generate(ctx, pool, o, sizes, results); err != nil {
		return err
	}
	if err := insertUsers(ctx, pool, results); err != nil {
		return err
	}
	slog.Info("trees loaded, rebuilding indexes and constraints", "elapsed", time.Since(start).Round(time.Second))

	if err := restoreSchema(ctx, pool, objs, o.workers); err != nil {
		return fmt.Errorf("rebuild schema (the database is unusable now, recreate it): %w", err)
	}
	// VACUUM, not only ANALYZE: it sets hint bits and the visibility map,
	// as on a long-running instance, so the first benchmark run doesn't pay
	// for rewriting every page it reads.
	vstart := time.Now()
	if _, err := pool.Exec(ctx, `VACUUM (ANALYZE) nodes, blobs, users, sessions`); err != nil {
		return fmt.Errorf("vacuum: %w", err)
	}
	slog.Info("vacuumed", "took", time.Since(vstart).Round(time.Second))
	if err := checkStats(ctx, pool, o.nodes); err != nil {
		return err
	}

	if err := writeTokens(o.tokensPath, results); err != nil {
		return err
	}
	var size string
	if err := pool.QueryRow(ctx, `SELECT pg_size_pretty(pg_database_size(current_database()))`).Scan(&size); err != nil {
		return err
	}
	slog.Info("done", "users", o.users, "nodes", o.nodes, "db_size", size, "tokens", o.tokensPath, "took", time.Since(start).Round(time.Second))
	return nil
}

// checkStats makes sure the planner sees about as many nodes as were
// loaded. Once, on the full 20M run, the VACUUM above left nodes with
// reltuples = Infinity; every estimate then overflowed and the search
// query's per-row path lookup became a sequential scan per row, while a
// fresh ANALYZE fixed it. Rare enough not to reproduce, bad enough that a
// benchmark on such statistics measures nothing: re-analyze, and refuse to
// finish if that doesn't help.
func checkStats(ctx context.Context, pool *pgxpool.Pool, want int) error {
	for attempt := 0; ; attempt++ {
		var got float64
		if err := pool.QueryRow(ctx, `SELECT reltuples FROM pg_class WHERE oid = 'nodes'::regclass`).Scan(&got); err != nil {
			return err
		}
		// ANALYZE samples, so the estimate is close, not exact.
		if !math.IsInf(got, 0) && !math.IsNaN(got) && math.Abs(got-float64(want)) <= 0.1*float64(want) {
			return nil
		}
		if attempt > 0 {
			return fmt.Errorf("nodes statistics are off after ANALYZE: reltuples %v, loaded %d", got, want)
		}
		slog.Warn("nodes statistics are off, analyzing again", "reltuples", got, "loaded", want)
		if _, err := pool.Exec(ctx, `ANALYZE nodes`); err != nil {
			return fmt.Errorf("analyze: %w", err)
		}
	}
}

func openPool(ctx context.Context, url string, conns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = conns
	// A lost tail of a bench load after a crash costs nothing — the run is
	// redone anyway — so don't wait for the WAL flush on every COPY.
	cfg.ConnConfig.RuntimeParams["synchronous_commit"] = "off"
	// Index builds sort 20M keys; the 64MB default spills them to disk.
	cfg.ConnConfig.RuntimeParams["maintenance_work_mem"] = "512MB"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	return pool, nil
}

// generate builds and copies every user's tree, o.workers at a time. Each
// user draws from its own random stream, so the rows don't depend on how
// many workers there are or which one got the user.
func generate(ctx context.Context, pool *pgxpool.Pool, o options, sizes []int, results []userResult) error {
	var done atomic.Int64
	next := make(chan int)
	errc := make(chan error, o.workers)
	var wg sync.WaitGroup
	for range o.workers {
		wg.Go(func() {
			for i := range next {
				r, err := loadUser(ctx, pool, o, i, sizes[i])
				if err != nil {
					errc <- fmt.Errorf("user %d: %w", i, err)
					return
				}
				results[i] = r
				done.Add(int64(sizes[i]))
			}
		})
	}

	stopProgress := make(chan struct{})
	go func() {
		start := time.Now()
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stopProgress:
				return
			case <-t.C:
				n := done.Load()
				slog.Info("progress", "nodes", n, "of", o.nodes, "per_sec", int(float64(n)/time.Since(start).Seconds()))
			}
		}
	}()
	defer close(stopProgress)

	var err error
feed:
	for i := range sizes {
		select {
		case next <- i:
		case err = <-errc:
			break feed
		case <-ctx.Done():
			err = ctx.Err()
			break feed
		}
	}
	close(next)
	wg.Wait()
	if err != nil {
		return err
	}
	select {
	case err := <-errc:
		return err
	default:
		return nil
	}
}

func loadUser(ctx context.Context, pool *pgxpool.Pool, o options, i, size int) (userResult, error) {
	rng := rand.New(rand.NewPCG(o.seed, uint64(i)))
	r := userResult{id: newUUID(rng), name: fmt.Sprintf("bench%04d", i+1)}
	var raw [32]byte
	randomBytes(rng, raw[:])
	r.token = base64.RawURLEncoding.EncodeToString(raw[:])

	spec := treeSpec{nodes: size, maxDepth: o.maxDepth, sampleDirs: o.sampleDirs}
	if i < o.bigDirs {
		spec.bigDir = o.bigDirSize
	}
	t, err := genTree(rng, spec)
	if err != nil {
		return userResult{}, err
	}
	if err := copyTree(ctx, pool, r.id, t); err != nil {
		return userResult{}, err
	}

	r.used = t.nodes[0].size
	r.line = tokenLine{UserID: r.id.String(), Username: r.name, Token: r.token, RootID: t.nodes[0].id.String()}
	if t.bigDir > 0 {
		r.line.BigDirID = t.nodes[t.bigDir].id.String()
	}
	for _, d := range t.sample {
		r.line.Dirs = append(r.line.Dirs, t.nodes[d].id.String())
	}
	return r, nil
}

func pgUUID(u uuid) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }

func copyTree(ctx context.Context, pool *pgxpool.Pool, owner uuid, t *tree) error {
	files := make([]int, 0, len(t.nodes))
	for i := range t.nodes {
		if !t.nodes[i].dir {
			files = append(files, i)
		}
	}
	_, err := pool.CopyFrom(ctx, pgx.Identifier{"blobs"},
		[]string{"id", "backend", "storage_key", "size", "hash", "created_at"},
		pgx.CopyFromSlice(len(files), func(k int) ([]any, error) {
			n := &t.nodes[files[k]]
			return []any{pgUUID(n.blobID), "s3", "bench/" + hex.EncodeToString(n.blobID[:]), n.size, n.hash[:], n.mtime}, nil
		}))
	if err != nil {
		return fmt.Errorf("copy blobs: %w", err)
	}

	ownerID := pgUUID(owner)
	_, err = pool.CopyFrom(ctx, pgx.Identifier{"nodes"},
		[]string{"id", "owner_id", "parent_id", "name", "name_key", "kind", "size", "mime", "blob_id", "content_hash", "etag", "mtime", "created_at", "updated_at"},
		pgx.CopyFromSlice(len(t.nodes), func(k int) ([]any, error) {
			n := &t.nodes[k]
			var parent, blob pgtype.UUID
			if n.parent >= 0 {
				parent = pgUUID(t.nodes[n.parent].id)
			}
			kind, mime, hash := "dir", any(nil), any(nil)
			if !n.dir {
				kind, mime, hash, blob = "file", n.mime, n.hash[:], pgUUID(n.blobID)
			}
			return []any{pgUUID(n.id), ownerID, parent, n.name, n.key, kind, n.size, mime, blob, hash,
				hex.EncodeToString(n.etag[:]), n.mtime, n.mtime, n.mtime}, nil
		}))
	if err != nil {
		return fmt.Errorf("copy nodes: %w", err)
	}
	return nil
}

// sessionTTL is far enough out that a dataset generated once stays usable
// for every later benchmark run.
const sessionTTL = 10 * 365 * 24 * time.Hour

func insertUsers(ctx context.Context, pool *pgxpool.Pool, results []userResult) error {
	_, err := pool.CopyFrom(ctx, pgx.Identifier{"users"},
		[]string{"id", "username", "display_name", "used_bytes"},
		pgx.CopyFromSlice(len(results), func(i int) ([]any, error) {
			r := &results[i]
			return []any{pgUUID(r.id), r.name, fmt.Sprintf("Bench User %d", i+1), r.used}, nil
		}))
	if err != nil {
		return fmt.Errorf("copy users: %w", err)
	}
	// Sessions as auth.Sessions.Create stores them: the SHA-256 of the
	// cookie value. last_seen_at defaults to now(), so a benchmark within
	// the first hour doesn't renew (write) the session on every request
	// either — the same as for a user who just signed in.
	expires := time.Now().Add(sessionTTL)
	_, err = pool.CopyFrom(ctx, pgx.Identifier{"sessions"},
		[]string{"id_hash", "user_id", "expires_at", "user_agent"},
		pgx.CopyFromSlice(len(results), func(i int) ([]any, error) {
			sum := sha256.Sum256([]byte(results[i].token))
			return []any{sum[:], pgUUID(results[i].id), expires, "benchgen"}, nil
		}))
	if err != nil {
		return fmt.Errorf("copy sessions: %w", err)
	}
	return nil
}

func writeTokens(path string, results []userResult) error {
	// 0600: the tokens are live sessions of the bench database.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("tokens file: %w", err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	for i := range results {
		if err := enc.Encode(results[i].line); err != nil {
			_ = f.Close()
			return fmt.Errorf("tokens file: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return fmt.Errorf("tokens file: %w", err)
	}
	return f.Close()
}
