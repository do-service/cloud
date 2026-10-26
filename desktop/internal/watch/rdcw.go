//go:build windows

package watch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// rdcw is the Watcher on Windows: one ReadDirectoryChangesW on the root
// with bWatchSubtree, so the whole tree costs one handle. fsnotify would
// open a handle per folder, and an open handle on a subfolder stops the
// user from renaming or deleting its parents. The root's own handle shares
// delete, so the root can still be renamed; that is noticed by asking the
// handle for its path (rootCheckInterval), as Windows reports nothing.
type rdcw struct {
	root    string
	h       windows.Handle
	latency time.Duration
	// ov.HEvent is signalled when a read completes; stop when Close is
	// called. The loop waits on both.
	ov   windows.Overlapped
	stop windows.Handle
	buf  []byte

	events  chan []Event
	done    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

// Variables for tests: a buffer too small for one entry makes every read
// an overflow, and the root check is slow to wait for.
var (
	// 64 KiB is the most a read on a network share may ask for.
	bufferSize        = 64 << 10
	rootCheckInterval = 5 * time.Second
)

// What ends up as Create, Remove, Rename and Modify; not last access,
// which reading a file changes, nor security descriptors.
const notifyFilter = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
	windows.FILE_NOTIFY_CHANGE_ATTRIBUTES | windows.FILE_NOTIFY_CHANGE_SIZE | windows.FILE_NOTIFY_CHANGE_LAST_WRITE

// New watches dir and everything below it. Options.Since is ignored:
// Windows keeps no history (the USN journal needs admin rights), so
// events while the client wasn't running are for the start-up scan to
// find. A file system that can't watch (some network shares) gives
// ErrUnsupported, for the engine to fall back to rescans.
func New(dir string, opts Options) (Watcher, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("watch: %w", err)
	}
	p, err := windows.UTF16PtrFromString(abs)
	if err != nil {
		return nil, fmt.Errorf("watch: %w", err)
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, fmt.Errorf("watch: %w", &fs.PathError{Op: "open", Path: abs, Err: err})
	}
	latency := opts.Latency
	if latency == 0 {
		latency = 200 * time.Millisecond
	}
	w := &rdcw{
		h: h, latency: latency, buf: make([]byte, bufferSize),
		events: make(chan []Event, 16), done: make(chan struct{}), stopped: make(chan struct{}),
	}
	fail := func(err error) (Watcher, error) {
		w.closeHandles()
		return nil, err
	}
	// The final path, not dir: symlinks, junctions, subst drives and 8.3
	// names resolved, so it's what the root check compares with.
	if w.root, err = finalPath(h); err != nil {
		return fail(fmt.Errorf("watch: %w", err))
	}
	if w.ov.HEvent, err = windows.CreateEvent(nil, 1, 0, nil); err != nil {
		return fail(fmt.Errorf("watch: %w", err))
	}
	if w.stop, err = windows.CreateEvent(nil, 1, 0, nil); err != nil {
		return fail(fmt.Errorf("watch: %w", err))
	}
	// Pinned for as long as the loop runs: the kernel writes into buf and
	// ov after the call that passed them has returned.
	var pin runtime.Pinner
	pin.Pin(&w.buf[0])
	pin.Pin(&w.ov)
	if err := w.read(); err != nil {
		pin.Unpin()
		if errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
			return fail(fmt.Errorf("%w: %s: %v", ErrUnsupported, abs, err))
		}
		return fail(fmt.Errorf("watch: %w", err))
	}
	go func() {
		defer pin.Unpin()
		w.loop()
	}()
	return w, nil
}

func (w *rdcw) Root() string { return w.root }

func (w *rdcw) Events() <-chan []Event { return w.events }

func (w *rdcw) Close() error {
	w.once.Do(func() {
		close(w.done)
		windows.SetEvent(w.stop)
		<-w.stopped
		w.closeHandles()
		close(w.events)
	})
	return nil
}

func (w *rdcw) closeHandles() {
	for _, h := range []windows.Handle{w.h, w.ov.HEvent, w.stop} {
		if h != 0 {
			windows.CloseHandle(h)
		}
	}
}

// read starts the next asynchronous read; ov.HEvent says when it's done.
func (w *rdcw) read() error {
	return windows.ReadDirectoryChanges(w.h, &w.buf[0], uint32(len(w.buf)), true, notifyFilter, nil, &w.ov, 0)
}

func (w *rdcw) loop() {
	defer close(w.stopped)
	var batch []Event
	var flushAt time.Time
	add := func(evs ...Event) {
		if len(evs) > 0 && batch == nil {
			flushAt = time.Now().Add(w.latency)
		}
		batch = append(batch, evs...)
	}
	// reading: a read is pending. After an error other than the root's
	// own, reading is retried every rootCheckInterval; after the root
	// changed, never again.
	reading, rootGone := true, false
	nextCheck := time.Now().Add(rootCheckInterval)
	for {
		wait := time.Until(nextCheck)
		if batch != nil {
			wait = min(wait, time.Until(flushAt))
		}
		handles := []windows.Handle{w.stop}
		if reading {
			handles = append(handles, w.ov.HEvent)
		}
		ev, err := windows.WaitForMultipleObjects(handles, false, uint32(max(wait, 0).Milliseconds()))
		switch {
		case err != nil:
			// Only invalid handles fail a wait, and they are ours: give up
			// watching, as after the root went away.
			reading, rootGone = w.cancel(reading), true
			add(Event{Path: w.root, Op: RootChanged})
		case ev == windows.WAIT_OBJECT_0:
			w.cancel(reading)
			return
		case ev == windows.WAIT_OBJECT_0+1:
			var n uint32
			err := windows.GetOverlappedResult(w.h, &w.ov, &n, false)
			windows.ResetEvent(w.ov.HEvent)
			again := true
			switch {
			case errors.Is(err, windows.ERROR_ACCESS_DENIED):
				// What a pending read gets when the root is deleted.
				again, rootGone = false, true
				add(Event{Path: w.root, Op: RootChanged})
			case errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR), err == nil && n == 0:
				// The buffer overflowed: what changed is lost.
				add(Event{Path: w.root, Op: MustRescan})
			case err != nil:
				// A share gone offline, say: tried again with the root check.
				again = false
				add(Event{Path: w.root, Op: MustRescan})
			default:
				add(w.parse(w.buf[:n])...)
			}
			reading = again && w.read() == nil
		case ev == uint32(windows.WAIT_TIMEOUT):
		}
		now := time.Now()
		if !now.Before(nextCheck) {
			nextCheck = now.Add(rootCheckInterval)
			if !rootGone {
				if p, err := finalPath(w.h); err != nil || !strings.EqualFold(p, w.root) {
					reading, rootGone = w.cancel(reading), true
					add(Event{Path: w.root, Op: RootChanged})
				} else if !reading && w.read() == nil {
					// Back after an error: what happened meanwhile is lost.
					reading = true
					add(Event{Path: w.root, Op: MustRescan})
				}
			}
		}
		if batch != nil && !now.Before(flushAt) {
			select {
			case w.events <- batch:
			case <-w.done:
				w.cancel(reading)
				return
			}
			batch = nil
		}
	}
}

// cancel stops a pending read and waits until the kernel lets go of the
// buffer. It returns false: nothing is pending any more.
func (w *rdcw) cancel(reading bool) bool {
	if reading {
		var n uint32
		windows.CancelIoEx(w.h, &w.ov)
		windows.GetOverlappedResult(w.h, &w.ov, &n, true)
	}
	return false
}

// parse turns the FILE_NOTIFY_INFORMATION records of one read into events.
func (w *rdcw) parse(buf []byte) []Event {
	var evs []Event
	for off := 0; off+int(unsafe.Offsetof(windows.FileNotifyInformation{}.FileName)) <= len(buf); {
		fni := (*windows.FileNotifyInformation)(unsafe.Pointer(&buf[off]))
		name := windows.UTF16ToString(unsafe.Slice(&fni.FileName, fni.FileNameLength/2))
		evs = append(evs, w.translate(filepath.Join(w.root, name), fni.Action))
		if fni.NextEntryOffset == 0 {
			break
		}
		off += int(fni.NextEntryOffset)
	}
	return evs
}

func (w *rdcw) translate(path string, action uint32) Event {
	e := Event{Path: path}
	switch action {
	case windows.FILE_ACTION_ADDED:
		e.Op = Create
	case windows.FILE_ACTION_REMOVED:
		e.Op = Remove
	case windows.FILE_ACTION_RENAMED_OLD_NAME, windows.FILE_ACTION_RENAMED_NEW_NAME:
		e.Op = Rename
	default:
		e.Op = Modify
	}
	// Inode stays 0: getting the file id means opening every file, and
	// the engine doesn't pair renames by it.
	fi, err := os.Lstat(path)
	if err != nil {
		return e
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		e.Op |= IsSymlink
	case fi.IsDir():
		e.Op |= IsDir
	default:
		e.Op |= IsFile
	}
	return e
}

// finalPath is where the handle's file is now, without the \\?\ prefix.
func finalPath(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]uint16, n)
	}
	p := windows.UTF16ToString(buf)
	if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
		return `\\` + rest, nil
	}
	return strings.TrimPrefix(p, `\\?\`), nil
}

// CurrentEventID is 0: ReadDirectoryChangesW has no event ids.
func CurrentEventID() uint64 { return 0 }
