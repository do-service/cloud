// Command cloudsync is the sync engine without the UI, for tests and
// dogfooding before the app is ready (docs/desktop-client.md):
//
//	cloudsync login [-name NAME] [-dir DIR] [-no-browser] <server-url>
//	cloudsync logout [-force] <server-url> [username]
//	cloudsync move-folder [-c CONN] <new-folder>
//	cloudsync accounts
//	cloudsync select [-c CONN] <path-in-cloud>
//	cloudsync unselect [-c CONN] <path-in-cloud>
//	cloudsync selection [-c CONN]
//	cloudsync sync [-once | -watch] [-confirm-deletes] [-poll 5s] [-limit-up KiB/s] [-limit-down KiB/s] [CONN]
//	cloudsync pause [CONN]
//	cloudsync resume [CONN]
//	cloudsync status [CONN]
//	cloudsync problems [CONN]
//	cloudsync watch [-since ID] <dir>
//
// login signs in in the browser, keeps the device token in the OS keyring
// and connects a sync folder (~/Cloud/<server> by default; files already
// in it are adopted, not uploaded); move-folder moves it to a new or
// empty folder on the same disk; select and
// unselect choose what syncs (nothing at first); sync runs the engine;
// pause and resume stop and restart it, also a running sync -watch.
// CONN is a connection's id, server or user@host, needed when there are
// several. Logs go to stderr (-v: more); tokens are never printed. watch
// is the FSEvents spike of E13-1: one line per event (id, ops, inode,
// path).
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/do-service/cloud/desktop/internal/watch"
)

func main() {
	args := os.Args[1:]
	level := slog.LevelInfo
	if len(args) > 0 && args[0] == "-v" {
		level, args = slog.LevelDebug, args[1:]
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	if len(args) < 1 {
		usage()
	}
	var err error
	switch args[0] {
	case "login":
		err = runLogin(args[1:])
	case "logout":
		err = runLogout(args[1:])
	case "accounts":
		err = runAccounts(args[1:])
	case "move-folder":
		err = runMoveFolder(args[1:])
	case "select":
		err = runSelect(args[1:], true)
	case "unselect":
		err = runSelect(args[1:], false)
	case "selection":
		err = runSelection(args[1:])
	case "sync":
		err = runSync(args[1:])
	case "pause":
		err = runPause(args[1:], true)
	case "resume":
		err = runPause(args[1:], false)
	case "status":
		err = runStatus(args[1:])
	case "problems":
		err = runProblems(args[1:])
	case "watch":
		err = runWatch(args[1:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cloudsync:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: cloudsync [-v] <command>
  cloudsync login [-name NAME] [-dir DIR] [-no-browser] <server-url>
  cloudsync logout [-force] <server-url> [username]
  cloudsync move-folder [-c CONN] <new-folder>
  cloudsync accounts
  cloudsync select [-c CONN] <path-in-cloud>
  cloudsync unselect [-c CONN] <path-in-cloud>
  cloudsync selection [-c CONN]
  cloudsync sync [-once | -watch] [-confirm-deletes] [-poll 5s] [-limit-up KiB/s] [-limit-down KiB/s] [CONN]
  cloudsync pause [CONN]
  cloudsync resume [CONN]
  cloudsync status [CONN]
  cloudsync problems [CONN]
  cloudsync watch [-since ID] <dir>
`)
	os.Exit(2)
}

func runWatch(args []string) error {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	since := fs.String("since", "now", "replay events after this id (from a previous run's last line)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		usage()
	}
	opts := watch.Options{Since: watch.SinceNow}
	if *since != "now" {
		id, err := strconv.ParseUint(*since, 10, 64)
		if err != nil {
			return fmt.Errorf("-since: %w", err)
		}
		opts.Since = id
	}
	w, err := watch.New(fs.Arg(0), opts)
	if errors.Is(err, watch.ErrUnsupported) {
		return fmt.Errorf("%w (built with CGO_ENABLED=0?)", err)
	}
	if err != nil {
		return err
	}
	defer w.Close()
	fmt.Fprintf(os.Stderr, "watching %s from %s, current id %d\n", w.Root(), *since, watch.CurrentEventID())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	for {
		select {
		case batch := <-w.Events():
			for _, e := range batch {
				fmt.Printf("%d\t%s\t%d\t%s\n", e.ID, e.Op, e.Inode, e.Path)
			}
		case <-stop:
			return nil
		}
	}
}
