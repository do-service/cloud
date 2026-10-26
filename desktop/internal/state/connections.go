package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/unicode/norm"
)

// RegistryFile is the connections registry's name in BaseDir.
const RegistryFile = "connections.json"

var (
	// ErrDuplicate: the same user on the same server is connected already.
	ErrDuplicate = errors.New("state: this account is connected already")
	// ErrNested: a sync folder would be inside another connection's, or
	// contain it; the two engines would sync each other's files.
	ErrNested = errors.New("state: sync folders of connections must not be nested")
)

// Connection is one server and user the client syncs (docs/desktop-client.md,
// «Подключения»). Each has its own state database at Path(ID), its own
// token in the keyring and its own sync folder.
type Connection struct {
	ID string `json:"id"`
	// Server is normalized (NormalizeServer).
	Server   string `json:"server"`
	UserID   string `json:"userId"`
	UserName string `json:"userName"`
	// LocalRoot is the sync folder, absolute.
	LocalRoot string `json:"localRoot"`
	// MovingTo is where the sync folder is being moved («Сменить папку»),
	// recorded before the move starts: after a crash midway the move is
	// finished or undone from here (connection.Open), rather than the
	// engine finding the folder gone. "" when no move is under way.
	MovingTo  string    `json:"movingTo,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// Registry is the list of connections, a small JSON file next to their
// state directories: read on start to know which engines to run, rarely
// written, and readable by a person debugging it. Not SQLite, since there
// is nothing to query and no transaction spanning it and a state.db.
//
// It serializes its own writes; a second running instance of the client
// is not guarded against here.
type Registry struct {
	path string
	// foldCase: whether sync folders are compared case-insensitively,
	// as the default file systems of macOS and Windows treat them.
	foldCase bool
	mu       sync.Mutex
}

// OpenRegistry uses dir/connections.json (dir is normally BaseDir()); a
// missing file is an empty registry, created on the first Add.
func OpenRegistry(dir string) *Registry {
	return &Registry{path: filepath.Join(dir, RegistryFile), foldCase: runtime.GOOS == "darwin" || runtime.GOOS == "windows"}
}

type registryFile struct {
	Connections []Connection `json:"connections"`
}

func (r *Registry) load() ([]Connection, error) {
	raw, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state: registry: %w", err)
	}
	var f registryFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("state: registry %s: %w", r.path, err)
	}
	return f.Connections, nil
}

// save replaces the file atomically: a crash leaves the old list or the
// new one, never a truncated file that would lose every connection.
func (r *Registry) save(conns []Connection) error {
	raw, err := json.MarshalIndent(registryFile{Connections: conns}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("state: registry: %w", err)
	}
	tmp, err := os.CreateTemp(dir, RegistryFile+".*.tmp")
	if err != nil {
		return fmt.Errorf("state: registry: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(raw, '\n')); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), r.path)
	}
	if err != nil {
		return fmt.Errorf("state: registry: %w", err)
	}
	return nil
}

// List returns the connections in the order they were added.
func (r *Registry) List() ([]Connection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.load()
}

func (r *Registry) Get(id string) (Connection, error) {
	conns, err := r.List()
	if err != nil {
		return Connection{}, err
	}
	for _, c := range conns {
		if c.ID == id {
			return c, nil
		}
	}
	return Connection{}, ErrNotFound
}

// Add registers a connection and returns it with Server normalized and
// ID and CreatedAt set. It refuses the same server and user twice
// (ErrDuplicate) and a sync folder nested with another one (ErrNested).
func (r *Registry) Add(c Connection) (Connection, error) {
	server, err := NormalizeServer(c.Server)
	if err != nil {
		return Connection{}, err
	}
	c.Server = server
	if c.UserID == "" {
		return Connection{}, errors.New("state: connection without a user id")
	}
	var id [8]byte
	rand.Read(id[:])
	c.ID = hex.EncodeToString(id[:])
	c.CreatedAt = time.Now().UTC().Truncate(time.Second)

	r.mu.Lock()
	defer r.mu.Unlock()
	conns, err := r.load()
	if err != nil {
		return Connection{}, err
	}
	for _, o := range conns {
		if o.Server == c.Server && o.UserID == c.UserID {
			return Connection{}, fmt.Errorf("%w: %s on %s", ErrDuplicate, c.UserName, c.Server)
		}
	}
	if err := r.checkRoot(conns, c); err != nil {
		return Connection{}, err
	}
	if err := r.save(append(conns, c)); err != nil {
		return Connection{}, err
	}
	return c, nil
}

// SetLocalRoot moves a connection's sync folder in the registry, with the
// same nesting check as Add.
func (r *Registry) SetLocalRoot(id, root string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	conns, err := r.load()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(conns, func(c Connection) bool { return c.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	c := conns[i]
	c.LocalRoot = root
	if err := r.checkRoot(conns, c); err != nil {
		return err
	}
	conns[i] = c
	return r.save(conns)
}

// CheckRoot is Add's and SetLocalRoot's check of a sync folder for
// connection id ("" for a new one), done before anything is changed.
func (r *Registry) CheckRoot(id, root string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	conns, err := r.load()
	if err != nil {
		return err
	}
	return r.checkRoot(conns, Connection{ID: id, LocalRoot: root})
}

// BeginMove records that connection id's sync folder is about to move
// to root, with the same check as Add: no other connection may take a
// folder nested with either place meanwhile.
func (r *Registry) BeginMove(id, root string) (Connection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	conns, err := r.load()
	if err != nil {
		return Connection{}, err
	}
	i := slices.IndexFunc(conns, func(c Connection) bool { return c.ID == id })
	if i < 0 {
		return Connection{}, ErrNotFound
	}
	if err := r.checkRoot(conns, Connection{ID: id, LocalRoot: root}); err != nil {
		return Connection{}, err
	}
	conns[i].MovingTo = root
	return conns[i], r.save(conns)
}

// EndMove ends a move BeginMove recorded: done, the connection's folder
// is now MovingTo; not done (undone), it stays where it was.
func (r *Registry) EndMove(id string, done bool) (Connection, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	conns, err := r.load()
	if err != nil {
		return Connection{}, err
	}
	i := slices.IndexFunc(conns, func(c Connection) bool { return c.ID == id })
	if i < 0 {
		return Connection{}, ErrNotFound
	}
	c := &conns[i]
	if c.MovingTo == "" {
		return *c, nil
	}
	if done {
		c.LocalRoot = c.MovingTo
	}
	c.MovingTo = ""
	return *c, r.save(conns)
}

// Remove forgets a connection. Its state directory (Dir(id)) and token
// are the caller's to delete, after its engine has stopped.
func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	conns, err := r.load()
	if err != nil {
		return err
	}
	i := slices.IndexFunc(conns, func(c Connection) bool { return c.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	return r.save(slices.Delete(conns, i, i+1))
}

func (r *Registry) checkRoot(conns []Connection, c Connection) error {
	if !filepath.IsAbs(c.LocalRoot) {
		return fmt.Errorf("state: sync folder %q is not an absolute path", c.LocalRoot)
	}
	// The state directory neither: a sync folder around it would sync
	// the databases, one inside it would go with a removed connection.
	if dir := filepath.Dir(r.path); Nested(dir, c.LocalRoot, r.foldCase) {
		return fmt.Errorf("%w: %s and the state directory %s", ErrNested, c.LocalRoot, dir)
	}
	for _, o := range conns {
		if o.ID == c.ID {
			continue
		}
		for _, root := range []string{o.LocalRoot, o.MovingTo} {
			if root != "" && Nested(root, c.LocalRoot, r.foldCase) {
				return fmt.Errorf("%w: %s and %s", ErrNested, root, c.LocalRoot)
			}
		}
	}
	return nil
}

// Nested reports whether a and b are the same folder or one is inside
// the other. Names are compared in NFC (macOS may hand back NFD) and, with
// foldCase, case-insensitively, as the file system would resolve them.
// Symlinks are not resolved: the paths are compared as chosen.
func Nested(a, b string, foldCase bool) bool {
	ka, kb := pathKey(a, foldCase), pathKey(b, foldCase)
	n := min(len(ka), len(kb))
	return slices.Equal(ka[:n], kb[:n])
}

func pathKey(p string, foldCase bool) []string {
	p = norm.NFC.String(filepath.Clean(p))
	if foldCase {
		p = strings.ToLower(p)
	}
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == filepath.Separator })
}

// NormalizeServer gives a server address one spelling, so one server
// isn't two connections: scheme and host lower-cased, the default port
// dropped, no trailing '/', no credentials, query or fragment. Plain http
// is only allowed to this machine (development); a token would otherwise
// travel in the clear.
func NormalizeServer(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("state: server address: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "https" && scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("state: server address %q: want http(s)://host", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("state: server address %q: no user, query or fragment", raw)
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if scheme == "http" && !loopback(host) {
		return "", fmt.Errorf("state: server address %q: http only for localhost, use https", raw)
	}
	hostport := host
	if strings.Contains(host, ":") {
		hostport = "[" + host + "]"
	}
	if port != "" {
		hostport = net.JoinHostPort(host, port)
	}
	return scheme + "://" + hostport + strings.TrimRight(u.EscapedPath(), "/"), nil
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
