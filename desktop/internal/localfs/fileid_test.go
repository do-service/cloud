package localfs

import "testing"

func TestFileIDBirthString(t *testing.T) {
	for _, c := range []struct {
		id FileID
		s  string
	}{
		{FileID{}, ""},
		{FileID{Volume: 0x4d, Index: 0x7b634}, "4d:7b634"},
		{FileID{Volume: 0x4d, Index: 0x7b634, Birth: 0x1869f3a2b}, "4d:7b634:1869f3a2b"},
	} {
		if got := c.id.String(); got != c.s {
			t.Errorf("%+v.String() = %q, want %q", c.id, got, c.s)
		}
		if got, err := ParseFileID(c.s); err != nil || got != c.id {
			t.Errorf("ParseFileID(%q) = %+v, %v", c.s, got, err)
		}
	}
	for _, s := range []string{"4d", "4d:x", "4d:1:2:3", "4d:1:zz"} {
		if _, err := ParseFileID(s); err == nil {
			t.Errorf("ParseFileID(%q) took it", s)
		}
	}
}

// A birth time tells a new file from a deleted one with its inode; one
// that isn't known (stored before, or not kept by the file system)
// doesn't make two ids differ.
func TestFileIDMatches(t *testing.T) {
	a := FileID{Volume: 1, Index: 2, Birth: 100}
	for _, c := range []struct {
		o    FileID
		want bool
	}{
		{a, true},
		{FileID{Volume: 1, Index: 2}, true},
		{FileID{Volume: 1, Index: 2, Birth: 101}, false},
		{FileID{Volume: 1, Index: 3, Birth: 100}, false},
		{FileID{Volume: 9, Index: 2}, false},
	} {
		if got := a.Matches(c.o); got != c.want || c.o.Matches(a) != c.want {
			t.Errorf("%+v matches %+v: %v", a, c.o, got)
		}
		if got := SameFileID(a.String(), c.o.String()); got != c.want {
			t.Errorf("SameFileID(%s, %s) = %v", a, c.o, got)
		}
	}
	if SameFileID("", "1:2") || SameFileID("1:2", "") || !SameFileID("", "") || SameFileID("bad", "1:2") {
		t.Error("SameFileID on empty or bad ids")
	}
	if FileIDKey("1:2:64") != "1:2" || FileIDKey("1:2") != "1:2" || FileIDKey("") != "" {
		t.Error("FileIDKey")
	}
}
