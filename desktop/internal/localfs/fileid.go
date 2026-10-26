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
type FileID struct {
	Volume uint64
	Index  uint64
}

func (id FileID) IsZero() bool { return id == FileID{} }

// String is how the state database stores it: "volume:index" in hex.
func (id FileID) String() string {
	if id.IsZero() {
		return ""
	}
	return strconv.FormatUint(id.Volume, 16) + ":" + strconv.FormatUint(id.Index, 16)
}

// ParseFileID reverses String; "" is the zero FileID.
func ParseFileID(s string) (FileID, error) {
	if s == "" {
		return FileID{}, nil
	}
	v, i, ok := strings.Cut(s, ":")
	if !ok {
		return FileID{}, fmt.Errorf("localfs: bad file id %q", s)
	}
	vol, err := strconv.ParseUint(v, 16, 64)
	if err != nil {
		return FileID{}, fmt.Errorf("localfs: bad file id %q", s)
	}
	idx, err := strconv.ParseUint(i, 16, 64)
	if err != nil {
		return FileID{}, fmt.Errorf("localfs: bad file id %q", s)
	}
	return FileID{Volume: vol, Index: idx}, nil
}
