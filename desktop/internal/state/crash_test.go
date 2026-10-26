package state

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// crashBatch is the number of base rows a helper transaction writes: big
// enough that a kill at a random moment usually lands inside one.
const crashBatch = 2000

// TestCrashHelper is the child process of TestKillMidTransaction; it does
// nothing in a normal run. It writes batches forever, each one transaction
// doing what the engine does: an op intent for batch i, its base rows, the
// previous op deleted, the cursor moved to i. With STATE_CRASH_HOLD it
// stops in the middle of a transaction and waits to be killed.
func TestCrashHelper(t *testing.T) {
	path := os.Getenv("STATE_CRASH_DB")
	if path == "" {
		t.Skip("helper process only")
	}
	hold := os.Getenv("STATE_CRASH_HOLD") != ""
	ctx := context.Background()
	db, err := Open(ctx, path)
	if err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	if err := db.Update(ctx, func(tx *Tx) error {
		_, err := tx.Account()
		if errors.Is(err, ErrNotFound) {
			return tx.SetAccount(Account{Server: "s", UserID: "u", DeviceID: "d", LocalRoot: "/r", Cursor: "-1"})
		}
		return err
	}); err != nil {
		fmt.Println("error", err)
		os.Exit(1)
	}
	fmt.Println("ready")
	for {
		var batch int
		err := db.Update(ctx, func(tx *Tx) error {
			a, err := tx.Account()
			if err != nil {
				return err
			}
			prev, _ := strconv.Atoi(a.Cursor)
			i := prev + 1
			batch = i
			if _, err := tx.AddOp(Op{Kind: "batch", Path: strconv.Itoa(i)}); err != nil {
				return err
			}
			for j := range crashBatch {
				if hold && j == crashBatch/2 {
					fmt.Println("mid")
					time.Sleep(time.Hour)
				}
				if err := tx.PutBase(Base{NodeID: fmt.Sprintf("%d-%d", i, j), RelPath: fmt.Sprintf("%d/%d", i, j), Kind: KindFile, ETag: "e"}); err != nil {
					return err
				}
			}
			if _, err := tx.exec(`DELETE FROM ops WHERE path = ?`, strconv.Itoa(prev)); err != nil {
				return err
			}
			return tx.SetCursor(strconv.Itoa(i))
		})
		if err != nil {
			fmt.Println("error", err)
			os.Exit(1)
		}
		fmt.Println("committed", batch)
	}
}

// A process killed with SIGKILL, mid-transaction or anywhere else, leaves
// a database that opens, passes integrity_check and holds whole
// transactions only: every batch up to the cursor complete, nothing of the
// next, exactly the cursor's op in the journal.
func TestKillMidTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	path := filepath.Join(t.TempDir(), FileName)
	last := -1
	for round := range 6 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCrashHelper$", "-test.count=1")
		cmd.Env = append(os.Environ(), "STATE_CRASH_DB="+path)
		hold := round == 0
		if hold {
			cmd.Env = append(cmd.Env, "STATE_CRASH_HOLD=1")
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		lines := bufio.NewScanner(out)
		waitFor := func(want string) {
			for lines.Scan() {
				if strings.HasPrefix(lines.Text(), want) {
					return
				}
				t.Logf("helper: %s", lines.Text())
			}
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatalf("helper exited before %q", want)
		}
		waitFor("ready")
		if hold {
			waitFor("mid")
		} else {
			// After one commit, so every round makes progress however
			// slow the machine (-race), then at a random moment.
			waitFor("committed")
			time.Sleep(time.Duration(rand.IntN(300)) * time.Millisecond)
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		cmd.Wait()

		last = checkCrashDB(t, path, last, !hold)
	}
}

// checkCrashDB verifies the invariants and returns the cursor. progress
// tells whether the round committed at least one batch; if not, the
// cursor must not have moved.
func checkCrashDB(t *testing.T, path string, prev int, progress bool) int {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	var cursor, rows, ops int
	var opPath string
	if err := db.View(ctx, func(tx *Tx) error {
		a, err := tx.Account()
		if err != nil {
			return err
		}
		cursor, _ = strconv.Atoi(a.Cursor)
		if err := tx.queryRow(`SELECT count(*) FROM base`).Scan(&rows); err != nil {
			return err
		}
		all, err := tx.Ops()
		ops = len(all)
		if len(all) == 1 {
			opPath = all[0].Path
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rows != (cursor+1)*crashBatch {
		t.Fatalf("cursor %d but %d base rows: a partial transaction survived", cursor, rows)
	}
	if cursor >= 0 && (ops != 1 || opPath != strconv.Itoa(cursor)) {
		t.Fatalf("cursor %d, journal has %d ops (%q)", cursor, ops, opPath)
	}
	if (progress && cursor <= prev) || (!progress && cursor != prev) {
		t.Fatalf("cursor went from %d to %d", prev, cursor)
	}
	t.Logf("after kill: cursor %d, %d rows", cursor, rows)
	return cursor
}
