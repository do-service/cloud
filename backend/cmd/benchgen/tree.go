package main

import (
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"time"

	"github.com/do-service/cloud/backend/internal/fs"
)

type uuid [16]byte

// newUUID is a random (version 4) uuid like gen_random_uuid() makes, but
// drawn from rng so the same seed gives the same dataset.
func newUUID(rng *rand.Rand) uuid {
	var u uuid
	for i := 0; i < 16; i += 8 {
		v := rng.Uint64()
		for j := range 8 {
			u[i+j] = byte(v >> (8 * j))
		}
	}
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return u
}

func (u uuid) String() string {
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], u[10:16])
	return string(b[:])
}

func randomBytes(rng *rand.Rand, b []byte) {
	for i := range b {
		if i%8 == 0 {
			v := rng.Uint64()
			for j := 0; j < 8 && i+j < len(b); j++ {
				b[i+j] = byte(v >> (8 * j))
			}
		}
	}
}

// baseTime anchors every generated timestamp, instead of time.Now, so a
// seed always produces the same rows.
var baseTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// mtimeSpan is how far back file modification times go.
const mtimeSpan = 6 * 365 * 24 * time.Hour

// node is one row of nodes (and, for a file, of blobs). Parents always
// come before their children in tree.nodes, which is what lets sizes be
// summed in one backwards pass.
type node struct {
	id     uuid
	parent int32 // index in tree.nodes, -1 for the root
	depth  uint8
	dir    bool
	name   string
	key    string
	mime   string
	size   int64
	mtime  time.Time
	etag   [16]byte
	blobID uuid
	hash   [32]byte
}

type tree struct {
	nodes []node
	// bigDir is the index of the folder with exactly spec.bigDir children,
	// 0 when there is none (0 is the root, which never is it).
	bigDir int
	// sample are indexes of folders to list in the workday scenario: the
	// root and a random pick of the rest, never bigDir or its subfolders.
	sample []int
}

type treeSpec struct {
	// nodes is the total, root and bigDir's subtree included.
	nodes int
	// bigDir > 0 adds a folder under the root with exactly that many
	// children (1% of them empty folders, so dirs-first paging is crossed).
	bigDir     int
	maxDepth   int
	sampleDirs int
}

// dirFraction is the share of folders among a user's nodes; Nextcloud
// instances typically show 5–15%.
const dirFraction = 0.1

// bigDirName is what the 50 000-item folder is called in every tree.
const bigDirName = "Большая папка"

// genTree builds one user's tree in memory: a random recursive tree of
// folders (every new folder hangs under a uniformly chosen existing one,
// which gives a few wide top levels and a depth around ln(folders), capped
// at maxDepth), then files spread over the folders with log-normal weights
// so most folders hold a handful of files and a few hold hundreds.
func genTree(rng *rand.Rand, spec treeSpec) (*tree, error) {
	regular := spec.nodes - 1
	if spec.bigDir > 0 {
		regular -= spec.bigDir + 1
	}
	if regular < 0 {
		return nil, fmt.Errorf("%d nodes is too few for a tree with a %d-item folder", spec.nodes, spec.bigDir)
	}
	t := &tree{nodes: make([]node, 0, spec.nodes)}
	// Names taken per folder, by name_key, so names differing only in case
	// are caught the way the unique index would catch them.
	taken := map[int32]map[string]struct{}{}

	add := func(parent int32, dir bool, name string) (int32, error) {
		raw := name
		name, key, err := fs.NormalizeName(raw)
		if err != nil {
			return 0, fmt.Errorf("generated name %q: %w", raw, err)
		}
		set := taken[parent]
		if set == nil {
			set = map[string]struct{}{}
			taken[parent] = set
		}
		set[key] = struct{}{}
		n := node{
			id:     newUUID(rng),
			parent: parent,
			depth:  t.nodes[parent].depth + 1,
			dir:    dir,
			name:   name,
			key:    key,
			mtime:  randomMTime(rng),
		}
		randomBytes(rng, n.etag[:])
		t.nodes = append(t.nodes, n)
		return int32(len(t.nodes) - 1), nil
	}
	uniqueIn := func(parent int32, name string) string {
		return dedupe(name, func(c string) bool {
			_, key, err := fs.NormalizeName(c)
			if err != nil {
				// Can't happen with the vocabularies here; let add report it.
				return false
			}
			_, ok := taken[parent][key]
			return ok
		})
	}

	root := node{id: newUUID(rng), parent: -1, dir: true, mtime: randomMTime(rng)}
	randomBytes(rng, root.etag[:])
	t.nodes = append(t.nodes, root)

	dirs := int(math.Round(float64(regular) * dirFraction))
	files := regular - dirs
	dirIdx := []int32{0}
	for range dirs {
		p := dirIdx[rng.IntN(len(dirIdx))]
		for int(t.nodes[p].depth) >= spec.maxDepth {
			p = t.nodes[p].parent
		}
		i, err := add(p, true, uniqueIn(p, dirName(rng)))
		if err != nil {
			return nil, err
		}
		dirIdx = append(dirIdx, i)
	}

	// Log-normal weights, then floor and hand out the remainder: the
	// counts add up to exactly files.
	weights := make([]float64, len(dirIdx))
	var total float64
	for i := range weights {
		weights[i] = math.Exp(rng.NormFloat64() * 1.3)
		total += weights[i]
	}
	counts := make([]int, len(dirIdx))
	given := 0
	for i, w := range weights {
		counts[i] = int(float64(files) * w / total)
		given += counts[i]
	}
	for ; given < files; given++ {
		counts[rng.IntN(len(counts))]++
	}
	for i, p := range dirIdx {
		for range counts[i] {
			ft := pickFileType(rng)
			j, err := add(p, false, uniqueIn(p, fileName(rng, ft)))
			if err != nil {
				return nil, err
			}
			setFile(rng, &t.nodes[j], ft)
		}
	}

	if spec.bigDir > 0 {
		big, err := add(0, true, uniqueIn(0, bigDirName))
		if err != nil {
			return nil, err
		}
		t.bigDir = int(big)
		subdirs := spec.bigDir / 100
		for k := range spec.bigDir {
			if k < subdirs {
				if _, err := add(big, true, uniqueIn(big, dirName(rng))); err != nil {
					return nil, err
				}
				continue
			}
			ft := pickFileType(rng)
			j, err := add(big, false, uniqueIn(big, fileName(rng, ft)))
			if err != nil {
				return nil, err
			}
			setFile(rng, &t.nodes[j], ft)
		}
	}

	// A folder's size is its subtree's total, as the fs service keeps it,
	// and its mtime is at least its newest child's. Children come after
	// their parent, so walking backwards finishes each folder before its
	// own parent reads it.
	for i := len(t.nodes) - 1; i > 0; i-- {
		n, p := &t.nodes[i], &t.nodes[t.nodes[i].parent]
		p.size += n.size
		if n.mtime.After(p.mtime) {
			p.mtime = n.mtime
		}
	}

	t.sample = []int{0}
	perm := rng.Perm(len(dirIdx) - 1)
	for _, k := range perm[:min(len(perm), max(spec.sampleDirs-1, 0))] {
		t.sample = append(t.sample, int(dirIdx[k+1]))
	}
	return t, nil
}

// randomMTime is at microsecond precision, what timestamptz keeps, so a
// cursor built from a generated row matches the stored value.
func randomMTime(rng *rand.Rand) time.Time {
	return baseTime.Add(-time.Duration(rng.Int64N(int64(mtimeSpan)))).Truncate(time.Microsecond)
}

func setFile(rng *rand.Rand, n *node, ft fileType) {
	n.mime = ft.mime
	n.size = fileSize(rng, ft)
	n.blobID = newUUID(rng)
	randomBytes(rng, n.hash[:])
}

// splitNodes divides total nodes among users with log-normal weights —
// a few heavy users, a long tail of light ones — keeping exactly total.
// Every user gets a root, and the first bigDirs users a bigDir-item
// folder on top of their share.
func splitNodes(rng *rand.Rand, users, total, bigDirs, bigDir int) ([]int, error) {
	if bigDirs > users {
		return nil, fmt.Errorf("%d big folders need at least as many users, got %d", bigDirs, users)
	}
	fixed := users + bigDirs*(bigDir+1)
	if total < fixed {
		return nil, fmt.Errorf("%d nodes is fewer than the %d that roots and big folders take", total, fixed)
	}
	rest := total - fixed
	weights := make([]float64, users)
	var sum float64
	for i := range weights {
		weights[i] = math.Exp(rng.NormFloat64())
		sum += weights[i]
	}
	out := make([]int, users)
	given := 0
	for i, w := range weights {
		share := int(float64(rest) * w / sum)
		given += share
		out[i] = 1 + share
		if i < bigDirs {
			out[i] += bigDir + 1
		}
	}
	for i := 0; given < rest; i, given = (i+1)%users, given+1 {
		out[i]++
	}
	return out, nil
}
