package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Bulk loading 20M rows through live indexes means a random B-tree insert
// per row and index (and a full-page WAL image for most of them), plus a
// trigram GIN update per name: hours instead of minutes. So the generator
// drops the indexes and constraints of nodes and blobs, loads, and rebuilds
// them from the definitions Postgres itself reports — whatever the
// migrations made them, nothing here repeats their DDL.
//
// The definitions are parked in pendingTable in the same transaction that
// drops them. If a run dies halfway the table is still there, and the next
// run refuses to start instead of loading into a schema without indexes.
const pendingTable = "benchgen_pending_schema"

type schemaObject struct {
	// phase orders the rebuild: keys before the indexes and foreign keys
	// that rely on them.
	phase int
	name  string
	sql   string
}

const (
	phaseKey = iota
	phaseIndex
	phaseForeignKey
)

// loadTables are the tables the generator fills in bulk.
const loadTables = `('nodes'::regclass, 'blobs'::regclass)`

func checkNoPending(ctx context.Context, pool *pgxpool.Pool) error {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, pendingTable).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("table %s exists: an earlier run was interrupted and left nodes/blobs without indexes; drop and recreate the database", pendingTable)
	}
	return nil
}

// dropForLoad drops the indexes and constraints of nodes and blobs, and
// foreign keys from elsewhere pointing at them, returning how to rebuild
// them. CHECK and NOT NULL constraints stay: they are cheap per row and
// catch a generator bug at COPY time.
func dropForLoad(ctx context.Context, pool *pgxpool.Pool) ([]schemaObject, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx, `
		SELECT CASE contype WHEN 'f' THEN 2 ELSE 0 END,
		       format('%I', conname),
		       format('ALTER TABLE %s ADD CONSTRAINT %I %s', conrelid::regclass, conname, pg_get_constraintdef(oid)),
		       format('ALTER TABLE %s DROP CONSTRAINT %I', conrelid::regclass, conname)
		FROM pg_constraint
		WHERE (contype IN ('p', 'u', 'x') AND conrelid IN `+loadTables+`)
		   OR (contype = 'f' AND (conrelid IN `+loadTables+` OR confrelid IN `+loadTables+`))
		UNION ALL
		SELECT 1, i.indexrelid::regclass::text, pg_get_indexdef(i.indexrelid),
		       format('DROP INDEX %s', i.indexrelid::regclass)
		FROM pg_index i
		WHERE i.indrelid IN `+loadTables+`
		  AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = i.indexrelid AND c.contype IN ('p', 'u', 'x'))
		-- Foreign keys first: a key can't be dropped while one depends on it.
		ORDER BY 1 DESC, 2`)
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	type dropped struct {
		obj  schemaObject
		drop string
	}
	ds, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dropped, error) {
		var d dropped
		err := r.Scan(&d.obj.phase, &d.obj.name, &d.obj.sql, &d.drop)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}

	if _, err := tx.Exec(ctx, `CREATE TABLE `+pendingTable+` (phase int NOT NULL, name text NOT NULL, sql text NOT NULL)`); err != nil {
		return nil, fmt.Errorf("create %s: %w", pendingTable, err)
	}
	objs := make([]schemaObject, 0, len(ds))
	for _, d := range ds {
		if _, err := tx.Exec(ctx, `INSERT INTO `+pendingTable+` VALUES ($1, $2, $3)`, d.obj.phase, d.obj.name, d.obj.sql); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, d.drop); err != nil {
			return nil, fmt.Errorf("%s: %w", d.drop, err)
		}
		objs = append(objs, d.obj)
	}
	return objs, tx.Commit(ctx)
}

// restoreSchema rebuilds what dropForLoad dropped, up to parallel
// statements at a time within a phase. Index builds take a SHARE lock and
// run side by side; keys and foreign keys lock harder, so those of one
// table queue behind each other, which is still correct.
func restoreSchema(ctx context.Context, pool *pgxpool.Pool, objs []schemaObject, parallel int) error {
	for phase := phaseKey; phase <= phaseForeignKey; phase++ {
		sem := make(chan struct{}, parallel)
		var wg sync.WaitGroup
		var mu sync.Mutex
		var errs []error
		for _, o := range objs {
			if o.phase != phase {
				continue
			}
			wg.Go(func() {
				sem <- struct{}{}
				defer func() { <-sem }()
				start := time.Now()
				if _, err := pool.Exec(ctx, o.sql); err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("%s: %w", o.sql, err))
					mu.Unlock()
					return
				}
				slog.Info("rebuilt", "object", o.name, "took", time.Since(start).Round(time.Second))
			})
		}
		wg.Wait()
		if err := errors.Join(errs...); err != nil {
			return err
		}
	}
	_, err := pool.Exec(ctx, `DROP TABLE `+pendingTable)
	return err
}
