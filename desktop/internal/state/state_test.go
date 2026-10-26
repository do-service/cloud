package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	// A space and Cyrillic in the path, as in "Application Support" and
	// a Russian user name.
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "Состояние "+FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func update(t *testing.T, db *DB, fn func(*Tx) error) {
	t.Helper()
	if err := db.Update(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

func view(t *testing.T, db *DB, fn func(*Tx) error) {
	t.Helper()
	if err := db.View(context.Background(), fn); err != nil {
		t.Fatal(err)
	}
}

func TestOpenSettings(t *testing.T) {
	db := openTemp(t)
	for pragma, want := range map[string]string{
		"journal_mode": "wal", "synchronous": "2", "foreign_keys": "1",
		"busy_timeout": fmt.Sprint(busyTimeoutMS), "user_version": "3",
	} {
		var got string
		if err := db.db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %s, want %s", pragma, got, want)
		}
	}
	if err := db.IntegrityCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestReopenAndTooNew(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	update(t, db, func(tx *Tx) error {
		return tx.SetAccount(Account{Server: "s", UserID: "u", DeviceID: "d", LocalRoot: "/r"})
	})
	db.Close()

	// Reopening runs no migration again and keeps the data.
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	view(t, db, func(tx *Tx) error {
		a, err := tx.Account()
		if err != nil || a.Server != "s" {
			t.Errorf("Account() = %+v, %v", a, err)
		}
		return nil
	})
	if _, err := db.db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Open(ctx, path); !errors.Is(err, ErrTooNew) {
		t.Fatalf("Open of a newer schema: %v, want ErrTooNew", err)
	}
	if _, err := Open(ctx, path+"?x"); err == nil {
		t.Fatal("Open accepted a path with '?'")
	}
}

func TestAccount(t *testing.T) {
	db := openTemp(t)
	view(t, db, func(tx *Tx) error {
		if _, err := tx.Account(); !errors.Is(err, ErrNotFound) {
			t.Errorf("Account() before SetAccount: %v", err)
		}
		return nil
	})
	if err := db.Update(context.Background(), func(tx *Tx) error { return tx.SetCursor("c") }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetCursor without account: %v", err)
	}
	want := Account{Server: "https://cloud.example", UserID: "u1", DeviceID: "d1", LocalRoot: "/Users/x/Cloud"}
	update(t, db, func(tx *Tx) error {
		if err := tx.SetAccount(want); err != nil {
			return err
		}
		if err := tx.SetCursor("cur-1"); err != nil {
			return err
		}
		// FSEvents ids use all 64 bits.
		return tx.SetWatchSince(^uint64(0) - 1)
	})
	want.Cursor, want.WatchSince = "cur-1", ^uint64(0)-1
	view(t, db, func(tx *Tx) error {
		got, err := tx.Account()
		if err != nil || got != want {
			t.Errorf("Account() = %+v, %v; want %+v", got, err, want)
		}
		return nil
	})
}

func TestRootsAndRules(t *testing.T) {
	db := openTemp(t)
	roots := []Root{
		{NodeID: "r1", Kind: RootOwn, OwnerID: "u1", OwnerName: "Я", Perms: ParsePerms([]string{"read", "write", "reshare"}), ETag: "e1"},
		{NodeID: "r2", Kind: RootShared, Name: "Отчёты (Иван)", OwnerID: "u2", OwnerName: "Иван", Perms: ParsePerms([]string{"read", "future"}), ETag: "e2"},
	}
	update(t, db, func(tx *Tx) error {
		if err := tx.ReplaceRoots(roots); err != nil {
			return err
		}
		if err := tx.ReplaceRoots(roots); err != nil {
			return err
		}
		if err := tx.SetRule("a", Include); err != nil {
			return err
		}
		if err := tx.SetRule("b", Include); err != nil {
			return err
		}
		if err := tx.SetRule("b", Exclude); err != nil {
			return err
		}
		if err := tx.SetRule("c", Include); err != nil {
			return err
		}
		return tx.DeleteRule("c")
	})
	view(t, db, func(tx *Tx) error {
		got, err := tx.Roots()
		if err != nil || len(got) != 2 || got[0] != roots[0] || got[1] != roots[1] {
			t.Errorf("Roots() = %+v, %v", got, err)
		}
		if got[1].Perms.Has(PermWrite) || !got[1].Perms.Has(PermRead) {
			t.Errorf("shared perms = %b", got[1].Perms)
		}
		rules, err := tx.Rules()
		if err != nil || len(rules) != 2 || rules["a"] != Include || rules["b"] != Exclude {
			t.Errorf("Rules() = %v, %v", rules, err)
		}
		return nil
	})
}

func TestRemote(t *testing.T) {
	db := openTemp(t)
	nodes := []Remote{
		{NodeID: "root", Name: "", NameKey: "", Kind: KindDir, ETag: "e0", MtimeNS: 1, Perms: PermRead | PermWrite},
		{NodeID: "a", ParentID: "root", Name: "Отчёт.docx", NameKey: "отчёт.docx", Kind: KindFile, Size: 10, ETag: "e1", SHA256: "ab", MtimeNS: 1_700_000_000_123_456_789, Perms: PermRead},
		{NodeID: "b", ParentID: "root", Name: "Dir", NameKey: "dir", Kind: KindDir, ETag: "e2", MtimeNS: 2},
	}
	update(t, db, func(tx *Tx) error {
		for _, n := range nodes {
			if err := tx.PutRemote(n); err != nil {
				return err
			}
		}
		nodes[1].ETag = "e1b"
		return tx.PutRemote(nodes[1])
	})
	view(t, db, func(tx *Tx) error {
		if got, err := tx.Remote("a"); err != nil || got != nodes[1] {
			t.Errorf("Remote(a) = %+v, %v", got, err)
		}
		if got, err := tx.Remote("root"); err != nil || got.ParentID != "" {
			t.Errorf("Remote(root) = %+v, %v", got, err)
		}
		if got, err := tx.RemoteChild("root", "dir"); err != nil || got.NodeID != "b" {
			t.Errorf("RemoteChild = %+v, %v", got, err)
		}
		kids, err := tx.RemoteChildren("root")
		if err != nil || len(kids) != 2 || kids[0].NodeID != "b" {
			t.Errorf("RemoteChildren = %+v, %v", kids, err)
		}
		if _, err := tx.Remote("zz"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Remote(zz): %v", err)
		}
		return nil
	})
	update(t, db, func(tx *Tx) error { return tx.DeleteRemote("a") })
	var n int
	view(t, db, func(tx *Tx) error { return tx.EachRemote(func(Remote) error { n++; return nil }) })
	if n != 2 {
		t.Fatalf("EachRemote saw %d", n)
	}
	update(t, db, func(tx *Tx) error { return tx.ClearRemote() })
	n = 0
	view(t, db, func(tx *Tx) error { return tx.EachRemote(func(Remote) error { n++; return nil }) })
	if n != 0 {
		t.Fatalf("after ClearRemote: %d", n)
	}
}

func TestLocalSubtreeDelete(t *testing.T) {
	db := openTemp(t)
	paths := []string{"a", "a/x", "a/y/z", "ab", "ab/c", "b", "Café/é"}
	update(t, db, func(tx *Tx) error {
		for i, p := range paths {
			if err := tx.PutLocal(Local{RelPath: p, Kind: KindFile, FileID: fmt.Sprintf("1:%d", i), SHA256Cached: "h"}); err != nil {
				return err
			}
		}
		return tx.DeleteLocal("a")
	})
	var left []string
	view(t, db, func(tx *Tx) error {
		if got, err := tx.LocalByFileID("1:3"); err != nil || len(got) != 1 || got[0].RelPath != "ab" {
			t.Errorf("LocalByFileID = %+v, %v", got, err)
		}
		return tx.EachLocal(func(l Local) error { left = append(left, l.RelPath); return nil })
	})
	if strings.Join(left, ",") != "ab,ab/c,b,Café/é" {
		t.Fatalf("left %v", left)
	}
	update(t, db, func(tx *Tx) error { return tx.DeleteLocal("") })
	view(t, db, func(tx *Tx) error {
		if _, err := tx.Local("b"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Local(b) after DeleteLocal(\"\"): %v", err)
		}
		return nil
	})
}

func TestLocalMove(t *testing.T) {
	db := openTemp(t)
	update(t, db, func(tx *Tx) error {
		for i, p := range []string{"a", "a/x", "a/y/z", "ab", "b", "b/old"} {
			if err := tx.PutLocal(Local{RelPath: p, Kind: KindFile, FileID: fmt.Sprintf("1:%d", i), SHA256Cached: "h"}); err != nil {
				return err
			}
		}
		// Onto b: what the scan saw at b is gone, a's subtree takes its place.
		return tx.MoveLocal("a", "b")
	})
	var got []string
	view(t, db, func(tx *Tx) error {
		return tx.EachLocal(func(l Local) error { got = append(got, l.RelPath+"="+l.FileID); return nil })
	})
	sort.Strings(got)
	if want := "ab=1:3,b/x=1:1,b/y/z=1:2,b=1:0"; strings.Join(got, ",") != want {
		t.Fatalf("after MoveLocal: %v, want %s", got, want)
	}
}

func TestBaseDeleteUnder(t *testing.T) {
	db := openTemp(t)
	update(t, db, func(tx *Tx) error {
		for i, p := range []string{"a", "a/x", "a/y/z", "ab"} {
			if err := tx.PutBase(Base{NodeID: fmt.Sprint(i), RelPath: p, Kind: KindFile, ETag: "e"}); err != nil {
				return err
			}
		}
		under, err := tx.BaseUnder("a")
		if err != nil || len(under) != 3 {
			t.Errorf("BaseUnder(a) = %+v, %v", under, err)
		}
		return tx.DeleteBaseUnder("a")
	})
	view(t, db, func(tx *Tx) error {
		for id, gone := range map[string]bool{"0": true, "1": true, "2": true, "3": false} {
			if _, err := tx.Base(id); errors.Is(err, ErrNotFound) != gone {
				t.Errorf("Base(%s): %v, want gone=%v", id, err, gone)
			}
		}
		return nil
	})
}

func TestBaseMove(t *testing.T) {
	db := openTemp(t)
	update(t, db, func(tx *Tx) error {
		for i, p := range []string{"Папка", "Папка/a.txt", "Папка/sub/b.txt", "Папка2/c.txt", "other"} {
			if err := tx.PutBase(Base{NodeID: fmt.Sprint(i), RelPath: p, Kind: KindFile, ETag: "e", FileID: fmt.Sprint(i)}); err != nil {
				return err
			}
		}
		return tx.MoveBase("Папка", "New/Folder")
	})
	view(t, db, func(tx *Tx) error {
		for id, want := range map[string]string{"0": "New/Folder", "1": "New/Folder/a.txt", "2": "New/Folder/sub/b.txt", "3": "Папка2/c.txt", "4": "other"} {
			if b, err := tx.Base(id); err != nil || b.RelPath != want {
				t.Errorf("Base(%s) = %+v, %v; want %s", id, b, err, want)
			}
		}
		if b, err := tx.BaseByPath("New/Folder/a.txt"); err != nil || b.NodeID != "1" {
			t.Errorf("BaseByPath = %+v, %v", b, err)
		}
		if bs, err := tx.BaseByFileID("4"); err != nil || len(bs) != 1 {
			t.Errorf("BaseByFileID = %+v, %v", bs, err)
		}
		return nil
	})
	// Two nodes can't be at one path.
	if err := db.Update(context.Background(), func(tx *Tx) error {
		return tx.PutBase(Base{NodeID: "9", RelPath: "other", Kind: KindFile, ETag: "e"})
	}); err == nil {
		t.Fatal("PutBase onto a taken path succeeded")
	}
	update(t, db, func(tx *Tx) error { return tx.DeleteBase("4") })
	view(t, db, func(tx *Tx) error {
		if _, err := tx.Base("4"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Base(4) after delete: %v", err)
		}
		return nil
	})
}

func TestIntendRunning(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	id, err := db.IntendRunning(ctx, Op{Kind: "upload", NodeID: "n", Path: "a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	view(t, db, func(tx *Tx) error {
		o, err := tx.Op(id)
		if err != nil || o.State != OpRunning || o.Attempts != 1 || o.Kind != "upload" {
			t.Errorf("op = %+v, %v", o, err)
		}
		return nil
	})
}

func TestOpsJournal(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.Intend(ctx, Op{Kind: "download", NodeID: "n1", Path: "a.txt", Params: json.RawMessage(`{"etag":"e1"}`)})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := db.Intend(ctx, Op{Kind: "upload", Path: "b.txt"})
	if err != nil {
		t.Fatal(err)
	}
	update(t, db, func(tx *Tx) error {
		if err := tx.StartOp(id); err != nil {
			return err
		}
		return tx.PutTransfer(Transfer{OpID: id, Done: 100, PartPath: "/r/.~cloud-n1.part"})
	})

	// A failing apply rolls back everything, the op stays.
	boom := errors.New("boom")
	if err := db.Complete(ctx, id, func(tx *Tx) error {
		if err := tx.PutBase(Base{NodeID: "n1", RelPath: "a.txt", Kind: KindFile, ETag: "e1"}); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("Complete: %v", err)
	}
	view(t, db, func(tx *Tx) error {
		if _, err := tx.Base("n1"); !errors.Is(err, ErrNotFound) {
			t.Errorf("base written by a failed Complete: %v", err)
		}
		return nil
	})

	// On restart, the journal lists the unfinished ops in order.
	db.Close()
	if db, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	view(t, db, func(tx *Tx) error {
		ops, err := tx.Ops()
		if err != nil || len(ops) != 2 {
			t.Fatalf("Ops() = %+v, %v", ops, err)
		}
		o := ops[0]
		if o.ID != id || o.State != OpRunning || o.Attempts != 1 || string(o.Params) != `{"etag":"e1"}` || o.CreatedAt.IsZero() {
			t.Errorf("op = %+v", o)
		}
		if string(ops[1].Params) != "{}" || ops[1].State != OpPending {
			t.Errorf("op2 = %+v", ops[1])
		}
		if x, err := tx.Transfer(id); err != nil || x.Done != 100 {
			t.Errorf("Transfer = %+v, %v", x, err)
		}
		return nil
	})

	update(t, db, func(tx *Tx) error { return tx.FailOp(id2, "locked", time.Now().Add(time.Hour)) })
	view(t, db, func(tx *Tx) error {
		due, err := tx.DueOps(time.Now(), 10)
		if err != nil || len(due) != 0 {
			t.Errorf("DueOps now = %+v, %v", due, err)
		}
		due, err = tx.DueOps(time.Now().Add(2*time.Hour), 10)
		if err != nil || len(due) != 1 || due[0].ID != id2 || due[0].ErrorCode != "locked" {
			t.Errorf("DueOps later = %+v, %v", due, err)
		}
		return nil
	})
	update(t, db, func(tx *Tx) error { return tx.RetryOp(id) })

	// Completing writes the result and drops the op and its transfer.
	if err := db.Complete(ctx, id, func(tx *Tx) error {
		return tx.PutBase(Base{NodeID: "n1", RelPath: "a.txt", Kind: KindFile, ETag: "e1"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Complete(ctx, id, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Complete: %v", err)
	}
	view(t, db, func(tx *Tx) error {
		if _, err := tx.Op(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("op after Complete: %v", err)
		}
		if _, err := tx.Transfer(id); !errors.Is(err, ErrNotFound) {
			t.Errorf("transfer after Complete: %v", err)
		}
		if _, err := tx.Base("n1"); err != nil {
			t.Errorf("base after Complete: %v", err)
		}
		return nil
	})
	if err := db.Update(ctx, func(tx *Tx) error { return tx.StartOp(12345) }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("StartOp of a missing op: %v", err)
	}
}

func TestProblemsAndActivity(t *testing.T) {
	db := openTemp(t)
	update(t, db, func(tx *Tx) error {
		id1, err := tx.PutProblem(Problem{Kind: "invalid_name", RelPath: "a:b", Code: "windows"})
		if err != nil {
			return err
		}
		id2, err := tx.PutProblem(Problem{Kind: "invalid_name", RelPath: "a:b", Code: "windows", Detail: "again"})
		if err != nil {
			return err
		}
		if id1 != id2 {
			t.Errorf("same problem got ids %d and %d", id1, id2)
		}
		if _, err := tx.PutProblem(Problem{Kind: "quota", NodeID: "n"}); err != nil {
			return err
		}
		for i := range ActivityLimit + 5 {
			if err := tx.AddActivity(Activity{Kind: "upload", RelPath: fmt.Sprint(i)}); err != nil {
				return err
			}
		}
		return nil
	})
	view(t, db, func(tx *Tx) error {
		ps, err := tx.Problems()
		if err != nil || len(ps) != 2 || ps[0].Detail != "again" {
			t.Errorf("Problems() = %+v, %v", ps, err)
		}
		all, err := tx.RecentActivity(ActivityLimit * 2)
		if err != nil || len(all) != ActivityLimit || all[0].RelPath != fmt.Sprint(ActivityLimit+4) || all[len(all)-1].RelPath != "5" {
			t.Errorf("RecentActivity: %d entries, %v", len(all), err)
		}
		return nil
	})
	update(t, db, func(tx *Tx) error { return tx.ResolveProblems("invalid_name", "a:b") })
	update(t, db, func(tx *Tx) error {
		ps, err := tx.Problems()
		if err != nil || len(ps) != 1 || ps[0].Kind != "quota" {
			t.Fatalf("Problems() after resolve = %+v, %v", ps, err)
		}
		return tx.DeleteProblem(ps[0].ID)
	})
}

// Writers from several goroutines wait for each other (BEGIN IMMEDIATE +
// busy timeout) instead of failing with SQLITE_BUSY, and readers see a
// consistent snapshot meanwhile.
func TestConcurrentWriters(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for w := range 8 {
		wg.Go(func() {
			for i := range 50 {
				if err := db.Update(ctx, func(tx *Tx) error {
					// Read, then write: the upgrade a deferred transaction
					// would fail on.
					if _, err := tx.Base(fmt.Sprint(w, "-", i)); !errors.Is(err, ErrNotFound) {
						return err
					}
					return tx.PutBase(Base{NodeID: fmt.Sprint(w, "-", i), RelPath: fmt.Sprint(w, "/", i), Kind: KindFile, ETag: "e"})
				}); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Go(func() {
		for range 50 {
			if err := db.View(ctx, func(tx *Tx) error { return tx.EachBase(func(Base) error { return nil }) }); err != nil {
				errs <- err
				return
			}
		}
	})
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var n int
	view(t, db, func(tx *Tx) error { return tx.EachBase(func(Base) error { n++; return nil }) })
	if n != 400 {
		t.Fatalf("%d rows, want 400", n)
	}
}
