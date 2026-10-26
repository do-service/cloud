package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/do-service/cloud/desktop/internal/connection"
	"github.com/do-service/cloud/desktop/internal/engine"
	"github.com/do-service/cloud/desktop/internal/state"
)

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// runSync: one sync to the end (-once, the default) or continuous
// (-watch: the watcher and a poll of the server's changes).
func runSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	once := fs.Bool("once", false, "sync until nothing is left to do, then exit (default without -watch)")
	watchMode := fs.Bool("watch", false, "keep syncing: local changes as they happen, the server's every -poll")
	confirm := fs.Bool("confirm-deletes", false, "go ahead with a mass delete the guard paused")
	poll := fs.Duration("poll", 5*time.Second, "the least time between two asks for the server's changes when idle")
	up := fs.Int("limit-up", 0, "upload limit, KiB/s (0: none)")
	down := fs.Int("limit-down", 0, "download limit, KiB/s (0: none)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	which, err := oneArg(fs.Args())
	if err != nil || *once && *watchMode || *up < 0 || *down < 0 {
		usage()
	}
	ctx, stop := signalContext()
	defer stop()
	s, err := openSession(ctx, which, connection.Options{ConfirmDeletes: *confirm, UpLimit: *up << 10, DownLimit: *down << 10,
		Tune: func(c *engine.Config) { c.PollInterval = *poll }})
	if err != nil {
		return err
	}
	defer s.Close()
	fmt.Fprintf(os.Stderr, "Синхронизация %s ↔ %s\n", connName(s.Conn), s.Conn.LocalRoot)
	if *watchMode {
		if s.Engine.Status().Paused {
			fmt.Fprintln(os.Stderr, "Синхронизация на паузе; продолжится после cloudsync resume.")
		}
		err := s.Engine.Run(ctx)
		return explain(err)
	}
	res, err := s.Engine.SyncOnce(ctx)
	if err != nil {
		return explain(err)
	}
	fmt.Printf("Готово: операций %d, не удалось %d, ждут повтора %d, проблем %d.\n", res.Ops, res.Failed, res.Pending, res.Problems)
	if res.HeldDeletes > 0 {
		fmt.Printf("Удаление %d файлов приостановлено. Если так и задумано: cloudsync sync -confirm-deletes\n", res.HeldDeletes)
	}
	return nil
}

// explain turns the engine's stop reasons into what to do.
func explain(err error) error {
	switch {
	case err == nil, errors.Is(err, context.Canceled):
		return nil
	case errors.Is(err, engine.ErrNeedsSignIn):
		return errors.New("устройство отключено на сервере; войдите заново: cloudsync login <адрес>")
	case errors.Is(err, engine.ErrPaused):
		return errors.New("синхронизация на паузе; продолжить: cloudsync resume")
	case errors.Is(err, engine.ErrNoMarker):
		return errors.New("папка синхронизации недоступна (нет файла .cloudsync) — диск отключён или папку перенесли; синхронизация не запускается, чтобы не удалить файлы в облаке")
	}
	return err
}

// runPause sets or clears the pause flag in the connection's state.db:
// a running cloudsync sync -watch picks it up within a second, and
// sync -once refuses to run while it is set.
func runPause(args []string, paused bool) error {
	which, err := oneArg(args)
	if err != nil {
		usage()
	}
	base, err := state.BaseDir()
	if err != nil {
		return err
	}
	c, err := pickConnection(base, which)
	if err != nil {
		return err
	}
	path, err := state.Path(c.ID)
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := state.Open(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx *state.Tx) error { return tx.SetPaused(paused) }); err != nil {
		return err
	}
	if paused {
		fmt.Printf("%s: синхронизация на паузе. Продолжить: cloudsync resume\n", connName(c))
	} else {
		fmt.Printf("%s: синхронизация продолжается.\n", connName(c))
	}
	return nil
}

func runSelect(args []string, include bool) error {
	name := "select"
	if !include {
		name = "unselect"
	}
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	which := fs.String("c", "", "connection (id, server or user@host); needed when there are several")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		usage()
	}
	ctx, stop := signalContext()
	defer stop()
	s, err := openSession(ctx, *which, connection.Options{})
	if err != nil {
		return err
	}
	defer s.Close()
	p := cloudPath(fs.Arg(0))
	if !include {
		if err := s.Engine.Unselect(ctx, p); err != nil {
			return explain(err)
		}
		fmt.Printf("Снята отметка: %s. Синхронизированные файлы уберутся с диска при следующей синхронизации (несохранённые изменения сначала загрузятся).\n", p)
		return nil
	}
	need, err := s.Engine.Select(ctx, p)
	if errors.Is(err, engine.ErrNoSpace) {
		return fmt.Errorf("не хватает места на диске для %s: нужно %s", p, size(need))
	}
	if err != nil {
		return explain(err)
	}
	fmt.Printf("Отмечено: %s (к скачиванию %s). Запустите cloudsync sync.\n", p, size(need))
	return nil
}

func runSelection(args []string) error {
	fs := flag.NewFlagSet("selection", flag.ExitOnError)
	which := fs.String("c", "", "connection")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()
	s, err := openSession(ctx, *which, connection.Options{})
	if err != nil {
		return err
	}
	defer s.Close()
	sel, err := s.Engine.Selection(ctx)
	if err != nil {
		return err
	}
	shares, err := engine.Shares(ctx, s.DB)
	if err != nil {
		return err
	}
	if len(sel) == 0 {
		fmt.Println("Ничего не отмечено: синхронизируется только то, что вы сами положите в папку. Отметить: cloudsync select <путь в облаке>")
	}
	for _, r := range sel {
		mode := "синхронизируется"
		if r.Mode == state.Exclude {
			mode = "исключено"
		}
		if r.Missing {
			mode += " (в облаке больше нет)"
		}
		fmt.Printf("%s\t%s\n", mode, r.Path)
	}
	if len(shares) > 0 {
		fmt.Println("Общие папки (отметить: cloudsync select \"<путь>\"):")
		printShares("  ", shares)
	}
	return nil
}

// printShares lists what others share with the user: path, owner,
// rights, whether it is synced.
func printShares(indent string, shares []engine.Share) {
	for _, sh := range shares {
		rights := "только чтение"
		if sh.Perms.Has(state.PermWrite) {
			rights = "чтение и запись"
		}
		synced := "не отмечена"
		if sh.Selected {
			synced = "синхронизируется"
		}
		fmt.Printf("%s%s\t(%s, %s, %s)\n", indent, sh.Path, sh.Owner, rights, synced)
	}
}

func runStatus(args []string) error {
	which, err := oneArg(args)
	if err != nil {
		usage()
	}
	base, err := state.BaseDir()
	if err != nil {
		return err
	}
	conns, err := state.OpenRegistry(base).List()
	if err != nil {
		return err
	}
	if which != "" {
		c, err := pickConnection(base, which)
		if err != nil {
			return err
		}
		conns = []state.Connection{c}
	}
	if len(conns) == 0 {
		fmt.Println("Нет подключений. Войдите: cloudsync login <адрес>")
		return nil
	}
	ctx := context.Background()
	for _, c := range conns {
		path, err := state.Path(c.ID)
		if err != nil {
			return err
		}
		db, err := state.Open(ctx, path)
		if err != nil {
			return err
		}
		shares, err := engine.Shares(ctx, db)
		if err != nil {
			db.Close()
			return err
		}
		var ops []state.Op
		var probs []state.Problem
		var act []state.Activity
		var acc state.Account
		var paused bool
		err = db.View(ctx, func(tx *state.Tx) error {
			var err error
			if acc, err = tx.Account(); err != nil {
				return err
			}
			if paused, err = tx.Paused(); err != nil {
				return err
			}
			if ops, err = tx.Ops(); err != nil {
				return err
			}
			if probs, err = tx.Problems(); err != nil {
				return err
			}
			act, err = tx.RecentActivity(1)
			return err
		})
		db.Close()
		if err != nil {
			return err
		}
		st := "в порядке"
		switch {
		case hasProblem(probs, "needs_signin"):
			st = "нужно войти заново"
		case hasProblem(probs, "no_marker"):
			st = "папка недоступна, синхронизация остановлена"
		case paused:
			st = "на паузе (cloudsync resume)"
		case hasProblem(probs, engine.ProblemOffline):
			st = "нет сети: сервер недоступен, повтор с нарастающей паузой"
		case hasProblem(probs, "mass_delete"):
			st = "удаление приостановлено, ждёт подтверждения"
		case acc.Cursor == "":
			st = "ещё не синхронизировалось"
		}
		fmt.Printf("%s  [%s]\n  папка:     %s\n  состояние: %s\n  в работе:  %d операций\n  проблемы:  %d\n", connName(c), c.ID, c.LocalRoot, st, len(ops), len(probs))
		if len(act) > 0 {
			fmt.Printf("  последнее: %s %s (%s)\n", act[0].Kind, act[0].RelPath, act[0].At.Local().Format("2006-01-02 15:04:05"))
		}
		if len(shares) > 0 {
			fmt.Printf("  общие:     %d\n", len(shares))
			printShares("    ", shares)
		}
	}
	return nil
}

func hasProblem(ps []state.Problem, kind string) bool {
	for _, p := range ps {
		if p.Kind == kind {
			return true
		}
	}
	return false
}

func runProblems(args []string) error {
	which, err := oneArg(args)
	if err != nil {
		usage()
	}
	base, err := state.BaseDir()
	if err != nil {
		return err
	}
	c, err := pickConnection(base, which)
	if err != nil {
		return err
	}
	path, err := state.Path(c.ID)
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := state.Open(ctx, path)
	if err != nil {
		return err
	}
	defer db.Close()
	var probs []state.Problem
	if err := db.View(ctx, func(tx *state.Tx) error {
		var err error
		probs, err = tx.Problems()
		return err
	}); err != nil {
		return err
	}
	if len(probs) == 0 {
		fmt.Println("Проблем нет.")
		return nil
	}
	for _, p := range probs {
		fmt.Printf("%s  %s\n", p.UpdatedAt.Local().Format("2006-01-02 15:04"), problemText(p))
	}
	return nil
}

func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f ГБ", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f МБ", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f КБ", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d Б", n)
}
