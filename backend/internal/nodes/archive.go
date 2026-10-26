package nodes

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/do-service/cloud/backend/internal/auth"
	"github.com/do-service/cloud/backend/internal/fs"
)

// archive streams a folder as a ZIP, written as it is read: nothing is
// staged on disk or held in memory beyond the list of paths.
//
// Entries are stored, not deflated: photos, video and office files are
// compressed already, and deflating would make a 2-vCPU server CPU-bound
// on a single download. archive/zip flags non-ASCII names as UTF-8 on its
// own; files of 4 GiB and more take the detour in addLargeFile.
func (h *Handlers) archive(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFromContext(r.Context())
	id, err := h.resolveID(r, u)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.streamArchive(w, r, u.ID, id)
}

// streamArchive writes folder id as a ZIP, as actorID sees it.
func (h *Handlers) streamArchive(w http.ResponseWriter, r *http.Request, actorID, id string) {
	ctx := r.Context()
	dir, err := h.FS.Get(ctx, actorID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	entries, err := h.FS.Walk(ctx, actorID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}

	name := dir.Name
	if dir.ParentID == "" {
		name = "files"
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/zip")
	hdr.Set("Content-Disposition", attachment(name+".zip"))
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cache-Control", "no-store")
	zw := zip.NewWriter(w)
	// From here on the response has started: a failure can only cut it
	// short, which leaves the archive without its central directory, so
	// no unzipper takes it for complete.
	abort := func(msg string, args ...any) {
		slog.WarnContext(ctx, "nodes: archive "+msg, append([]any{"dir", dir.ID}, args...)...)
		panic(http.ErrAbortHandler)
	}
	for _, e := range entries {
		fh := &zip.FileHeader{Name: e.Path, Method: zip.Store, Modified: e.MTime}
		if e.Kind == fs.KindDir {
			// Kept so empty folders survive the round trip.
			fh.Name += "/"
			if _, err := zw.CreateHeader(fh); err != nil {
				abort("interrupted", "err", err)
			}
			continue
		}
		var err error
		if e.Size >= zip64Size {
			err = h.addLargeFile(ctx, zw, fh, actorID, e.ID)
		} else {
			err = h.addFile(ctx, zw, fh, actorID, e.ID)
		}
		if errors.Is(err, fs.ErrNotFound) {
			// Deleted since the walk; the rest of the folder still counts.
			slog.InfoContext(ctx, "nodes: archive skips a file deleted meanwhile", "node", e.ID)
			continue
		}
		if err != nil {
			abort("interrupted", "node", e.ID, "err", err)
		}
	}
	if err := zw.Close(); err != nil {
		abort("interrupted", "err", err)
	}
}

// zip64Size is where an entry's sizes no longer fit the 32-bit fields. A
// variable so tests can send small files down that path.
var zip64Size int64 = 1 << 32

// extTimeExtraID is the extended timestamp extra field (Info-ZIP's "UT").
const extTimeExtraID = 0x5455

var errChanged = errors.New("file changed while being archived")

func (h *Handlers) addFile(ctx context.Context, zw *zip.Writer, fh *zip.FileHeader, actorID, id string) error {
	rc, _, err := h.FS.Open(ctx, actorID, id, nil)
	if err != nil {
		return err
	}
	defer rc.Close()
	fw, err := zw.CreateHeader(fh)
	if err != nil {
		return err
	}
	_, err = io.Copy(fw, rc)
	return err
}

// addLargeFile writes an entry of 4 GiB or more with its CRC-32 and sizes
// in the local header, which takes reading the content twice. Streamed
// the usual way, the local header has neither and no ZIP64 field either,
// and macOS's Archive Utility then misreads the ZIP64 data descriptor
// and rejects the archive; the CRC is the only part not known upfront.
func (h *Handlers) addLargeFile(ctx context.Context, zw *zip.Writer, fh *zip.FileHeader, actorID, id string) error {
	rc, first, err := h.FS.Open(ctx, actorID, id, nil)
	if err != nil {
		return err
	}
	sum := crc32.NewIEEE()
	_, err = io.Copy(sum, rc)
	rc.Close()
	if err != nil {
		return err
	}
	rc, second, err := h.FS.Open(ctx, actorID, id, nil)
	if err != nil {
		return err
	}
	defer rc.Close()
	if second.Size != first.Size || !bytes.Equal(second.ContentHash, first.ContentHash) {
		return errChanged
	}
	fh.CRC32 = sum.Sum32()
	fh.CompressedSize64 = uint64(first.Size)
	fh.UncompressedSize64 = uint64(first.Size)
	// What CreateHeader would derive and CreateRaw doesn't: the UTF-8
	// name flag, the MS-DOS timestamp, and the extended timestamp field
	// with the Unix time, since the MS-DOS one has no time zone.
	if strings.ContainsFunc(fh.Name, func(r rune) bool { return r >= utf8.RuneSelf }) {
		fh.Flags |= 0x800
	}
	t := fh.Modified
	fh.ModifiedDate = uint16(t.Day() + int(t.Month())<<5 + (t.Year()-1980)<<9)
	fh.ModifiedTime = uint16(t.Second()/2 + t.Minute()<<5 + t.Hour()<<11)
	fh.Extra = binary.LittleEndian.AppendUint16(fh.Extra, extTimeExtraID)
	fh.Extra = binary.LittleEndian.AppendUint16(fh.Extra, 5)
	fh.Extra = append(fh.Extra, 1) // flags: modification time only
	fh.Extra = binary.LittleEndian.AppendUint32(fh.Extra, uint32(t.Unix()))
	fw, err := zw.CreateRaw(fh)
	if err != nil {
		return err
	}
	// CreateRaw trusts the header: a short read must not pass for the
	// whole file.
	n, err := io.Copy(fw, rc)
	if err == nil && n != first.Size {
		err = errChanged
	}
	return err
}
