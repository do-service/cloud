package localfs

import (
	"fmt"
	"strconv"
	"strings"
)

// FileID identifies a file on this machine across renames: device and
// inode on Unix, volume serial and file index on Windows. A rename keeps
// it; an editor's "save as temp, rename over" gives a new one, which is
// how a replaced file is told from a moved one.
//
// Linux file systems hand a freed inode number to the next new file at
// once (ext4 does), so there device and inode alone can take a new file
// for a deleted one: Birth, the file's creation time (statx), tells them
// apart. Elsewhere it is 0: APFS and HFS+ don't reuse inode numbers, and
// NTFS's file index carries a sequence number that changes on reuse.
type FileID struct {
	Volume uint64
	Index  uint64
	// Birth is the creation time in Unix nanoseconds; 0 when unknown (the
	// file system doesn't keep it, or an id stored before it was read).
	Birth int64
}

func (id FileID) IsZero() bool { return id == FileID{} }

// Matches: the same file, as far as the two ids tell. A Birth of 0 on
// either side is unknown, not different: then volume and index decide,
// as they did before Birth was read.
func (id FileID) Matches(o FileID) bool {
	return id.Volume == o.Volume && id.Index == o.Index && (id.Birth == o.Birth || id.Birth == 0 || o.Birth == 0)
}

// String is how the state database stores it: "volume:index" in hex, and
// ":birth" when known.
func (id FileID) String() string {
	if id.IsZero() {
		return ""
	}
	s := strconv.FormatUint(id.Volume, 16) + ":" + strconv.FormatUint(id.Index, 16)
	if id.Birth != 0 {
		s += ":" + strconv.FormatInt(id.Birth, 16)
	}
	return s
}

// ParseFileID reverses String; "" is the zero FileID.
func ParseFileID(s string) (FileID, error) {
	if s == "" {
		return FileID{}, nil
	}
	bad := fmt.Errorf("localfs: bad file id %q", s)
	parts := strings.Split(s, ":")
	if len(parts) != 2 && len(parts) != 3 {
		return FileID{}, bad
	}
	vol, err := strconv.ParseUint(parts[0], 16, 64)
	if err != nil {
		return FileID{}, bad
	}
	idx, err := strconv.ParseUint(parts[1], 16, 64)
	if err != nil {
		return FileID{}, bad
	}
	id := FileID{Volume: vol, Index: idx}
	if len(parts) == 3 {
		if id.Birth, err = strconv.ParseInt(parts[2], 16, 64); err != nil {
			return FileID{}, bad
		}
	}
	return id, nil
}

// SameFileID is Matches on two stored ids; unreadable ones match only
// themselves.
func SameFileID(a, b string) bool {
	if a == b {
		return true
	}
	x, err := ParseFileID(a)
	if err != nil {
		return false
	}
	y, err := ParseFileID(b)
	return err == nil && !x.IsZero() && x.Matches(y)
}

// FileIDKey is a stored id without its Birth: ids that may Match share
// it.
func FileIDKey(s string) string {
	if i := strings.LastIndexByte(s, ':'); i >= 0 && strings.Count(s, ":") == 2 {
		return s[:i]
	}
	return s
}
