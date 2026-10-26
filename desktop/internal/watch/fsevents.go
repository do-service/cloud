//go:build darwin && cgo

package watch

/*
#cgo LDFLAGS: -framework CoreServices
#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <stdlib.h>
#include "fsevents.h"

// UseCFTypes + UseExtendedData: each event comes as a dictionary with the
// path and the file id (inode), which the engine needs to pair renames.
static const FSEventStreamCreateFlags cs_flags =
	kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagWatchRoot |
	kFSEventStreamCreateFlagUseCFTypes | kFSEventStreamCreateFlagUseExtendedData;

// The path is copied as UTF-8 exactly as FSEvents has it, not through
// CFStringGetFileSystemRepresentation, which would decompose it (NFD) and
// so change names the engine compares with the server's.
static char *cs_cstring(CFStringRef s) {
	if (s == NULL) return NULL;
	CFIndex size = CFStringGetMaximumSizeForEncoding(CFStringGetLength(s), kCFStringEncodingUTF8) + 1;
	char *buf = malloc(size);
	if (buf != NULL && !CFStringGetCString(s, buf, size, kCFStringEncodingUTF8)) {
		free(buf);
		return NULL;
	}
	return buf;
}

static void cs_callback(ConstFSEventStreamRef stream, void *info, size_t n, void *paths,
		const FSEventStreamEventFlags flags[], const FSEventStreamEventId ids[]) {
	CFArrayRef arr = (CFArrayRef)paths;
	cs_event *evs = calloc(n, sizeof(cs_event));
	if (evs == NULL) return;
	for (size_t i = 0; i < n; i++) {
		CFDictionaryRef d = (CFDictionaryRef)CFArrayGetValueAtIndex(arr, (CFIndex)i);
		evs[i].path = cs_cstring((CFStringRef)CFDictionaryGetValue(d, kFSEventStreamEventExtendedDataPathKey));
		evs[i].flags = flags[i];
		evs[i].id = ids[i];
		// Absent for history-done, root-changed and dropped-event markers.
		CFNumberRef ino = (CFNumberRef)CFDictionaryGetValue(d, kFSEventStreamEventExtendedFileIDKey);
		int64_t v = 0;
		if (ino != NULL && CFNumberGetValue(ino, kCFNumberSInt64Type, &v)) evs[i].inode = (uint64_t)v;
	}
	watchGoCallback((uintptr_t)info, evs, n);
	for (size_t i = 0; i < n; i++) free(evs[i].path);
	free(evs);
}

static FSEventStreamRef cs_create(uintptr_t handle, const char *path, FSEventStreamEventId since, double latency) {
	CFStringRef p = CFStringCreateWithCString(NULL, path, kCFStringEncodingUTF8);
	if (p == NULL) return NULL;
	CFArrayRef paths = CFArrayCreate(NULL, (const void **)&p, 1, &kCFTypeArrayCallBacks);
	FSEventStreamContext ctx = {0, (void *)handle, NULL, NULL, NULL};
	FSEventStreamRef s = FSEventStreamCreate(NULL, cs_callback, &ctx, paths, since, latency, cs_flags);
	CFRelease(paths);
	CFRelease(p);
	return s;
}

// A dispatch queue rather than a run loop: the run-loop API is deprecated,
// and a queue needs no thread of ours parked in CFRunLoopRun.
static dispatch_queue_t cs_queue(void) {
	return dispatch_queue_create("ru.exz-vps.cloud.watch", DISPATCH_QUEUE_SERIAL);
}

static Boolean cs_start(FSEventStreamRef s, dispatch_queue_t q) {
	FSEventStreamSetDispatchQueue(s, q);
	return FSEventStreamStart(s);
}

static void cs_noop(void *ctx) {}

static void cs_stop(FSEventStreamRef s, dispatch_queue_t q, Boolean started) {
	if (started) FSEventStreamStop(s);
	FSEventStreamInvalidate(s);
	// Invalidate stops new callbacks; draining the serial queue waits for
	// one already running, so the Go handle can be freed after this.
	dispatch_sync_f(q, NULL, cs_noop);
	FSEventStreamRelease(s);
	dispatch_release(q);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime/cgo"
	"sync"
	"time"
	"unsafe"
)

// fsevents is the Watcher on macOS: one stream for the whole tree.
type fsevents struct {
	root   string
	events chan []Event
	done   chan struct{}
	stream C.FSEventStreamRef
	queue  C.dispatch_queue_t
	handle cgo.Handle
	once   sync.Once
}

// New watches dir and everything below it. With opts.Since set to a saved
// event id it first replays what happened after it, then a HistoryDone
// event, then live events.
func New(dir string, opts Options) (Watcher, error) {
	// FSEvents reports real paths: resolving here gives Root the same form.
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, fmt.Errorf("watch: %w", err)
	}
	if root, err = filepath.Abs(root); err != nil {
		return nil, fmt.Errorf("watch: %w", err)
	}
	latency := opts.Latency
	if latency == 0 {
		latency = 200 * time.Millisecond
	}
	since := opts.Since
	if since == 0 {
		// 0 would mean "since the volume's first event", a replay of
		// everything ever; nobody means that.
		since = SinceNow
	}

	w := &fsevents{root: root, events: make(chan []Event, 16), done: make(chan struct{})}
	w.handle = cgo.NewHandle(w)
	cpath := C.CString(root)
	defer C.free(unsafe.Pointer(cpath))
	w.stream = C.cs_create(C.uintptr_t(w.handle), cpath, C.FSEventStreamEventId(since), C.double(latency.Seconds()))
	if w.stream == nil {
		w.handle.Delete()
		return nil, errors.New("watch: FSEventStreamCreate failed")
	}
	w.queue = C.cs_queue()
	if C.cs_start(w.stream, w.queue) == 0 {
		C.cs_stop(w.stream, w.queue, 0)
		w.handle.Delete()
		return nil, errors.New("watch: FSEventStreamStart failed")
	}
	return w, nil
}

// Root is the watched folder with symlinks resolved, the prefix of every
// Event.Path.
func (w *fsevents) Root() string { return w.root }

// Events yields batches until Close; it is closed then.
func (w *fsevents) Events() <-chan []Event { return w.events }

// Close stops the stream. Batches not yet received are dropped: the
// caller resumes from the last ID it handled.
func (w *fsevents) Close() error {
	w.once.Do(func() {
		// First, so a callback blocked on a full channel returns and the
		// queue can drain in cs_stop.
		close(w.done)
		C.cs_stop(w.stream, w.queue, 1)
		w.handle.Delete()
		close(w.events)
	})
	return nil
}

// deliver runs on the stream's dispatch queue. Blocking here when the
// consumer is slow is the backpressure we want: FSEvents keeps queueing in
// the kernel and says MustRescan if it ever has to drop.
func (w *fsevents) deliver(batch []Event) {
	select {
	case w.events <- batch:
	case <-w.done:
	}
}

// CurrentEventID is the id of the latest event on the system: saved before
// a scan, it lets a later watcher replay everything the scan may have
// missed.
func CurrentEventID() uint64 {
	return uint64(C.FSEventsGetCurrentEventId())
}

func toOp(f uint32) Op {
	var op Op
	for _, m := range []struct {
		flag uint32
		op   Op
	}{
		{C.kFSEventStreamEventFlagItemCreated, Create},
		{C.kFSEventStreamEventFlagItemRemoved, Remove},
		{C.kFSEventStreamEventFlagItemRenamed, Rename},
		{C.kFSEventStreamEventFlagItemModified, Modify},
		{C.kFSEventStreamEventFlagItemInodeMetaMod | C.kFSEventStreamEventFlagItemChangeOwner |
			C.kFSEventStreamEventFlagItemXattrMod | C.kFSEventStreamEventFlagItemFinderInfoMod, Meta},
		{C.kFSEventStreamEventFlagItemIsDir, IsDir},
		{C.kFSEventStreamEventFlagItemIsFile, IsFile},
		{C.kFSEventStreamEventFlagItemIsSymlink, IsSymlink},
		// Event ids wrapping (after 2^64) invalidates any saved id: as
		// good as dropped events.
		{C.kFSEventStreamEventFlagMustScanSubDirs | C.kFSEventStreamEventFlagUserDropped |
			C.kFSEventStreamEventFlagKernelDropped | C.kFSEventStreamEventFlagEventIdsWrapped, MustRescan},
		{C.kFSEventStreamEventFlagHistoryDone, HistoryDone},
		{C.kFSEventStreamEventFlagRootChanged, RootChanged},
	} {
		if f&m.flag != 0 {
			op |= m.op
		}
	}
	return op
}
