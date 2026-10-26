package fs

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"testing"
)

var updateVectors = flag.Bool("update", false, "rewrite testdata/name_vectors.json from NormalizeName")

// nameVector is a case of testdata/name_vectors.json, the golden file the
// desktop client checks its port of NormalizeName against
// (desktop/internal/localfs): both sides must agree on names and name_key,
// or the client would see conflicts the server doesn't, and miss others.
type nameVector struct {
	Note string `json:"note"`
	In   string `json:"in,omitempty"`
	// InHex is the input when it isn't valid UTF-8, which JSON can't carry.
	InHex string `json:"inHex,omitempty"`
	Valid bool   `json:"valid"`
	Name  string `json:"name,omitempty"`
	Key   string `json:"key,omitempty"`
}

func (v nameVector) input(t *testing.T) string {
	if v.InHex == "" {
		return v.In
	}
	b, err := hex.DecodeString(v.InHex)
	if err != nil {
		t.Fatalf("%s: inHex: %v", v.Note, err)
	}
	return string(b)
}

// To add a case: append it with note and in (or inHex), run
// go test ./internal/fs -run NameVectors -update, review the diff.
func TestNameVectors(t *testing.T) {
	const path = "testdata/name_vectors.json"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vectors []nameVector
	if err := json.Unmarshal(raw, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) < 30 {
		t.Fatalf("only %d vectors", len(vectors))
	}
	for i, v := range vectors {
		name, key, err := NormalizeName(v.input(t))
		if err != nil && !errors.Is(err, ErrInvalidName) {
			t.Fatalf("%s: %v", v.Note, err)
		}
		got := nameVector{Note: v.Note, In: v.In, InHex: v.InHex, Valid: err == nil, Name: name, Key: key}
		if *updateVectors {
			vectors[i] = got
		} else if got != v {
			t.Errorf("%s: NormalizeName(%q) = (%q, %q, valid %v), file says (%q, %q, valid %v)",
				v.Note, v.input(t), name, key, got.Valid, v.Name, v.Key, v.Valid)
		}
	}
	if !*updateVectors {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// Names like a<b&c stay readable in the file.
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(vectors); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
