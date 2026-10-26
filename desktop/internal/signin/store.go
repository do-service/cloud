package signin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/do-service/cloud/desktop/internal/api"
)

// KeyringService names the client's entries in Keychain, Credential
// Manager or Secret Service.
const KeyringService = "Cloud"

const (
	// AccountsFile lists the signed-in accounts, without tokens: the
	// keyring can't be listed. The connections registry
	// (state.Registry, connections.json) supersedes it once the app
	// creates connections with their folders.
	AccountsFile = "accounts.json"
	// TokensFile holds tokens only where the keyring is unavailable
	// (Linux without a Secret Service): mode 0600, like ~/.ssh keys.
	TokensFile = "tokens.json"
)

var ErrNoToken = errors.New("signin: no token stored for this account")

// Account is a signed-in account, as AccountsFile has it.
type Account struct {
	// Server is normalized.
	Server      string `json:"server"`
	UserID      string `json:"userId"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName,omitempty"`
	DeviceID    string `json:"deviceId"`
	DeviceName  string `json:"deviceName"`
	// TokenInFile: the keyring was unavailable at sign-in, so the token
	// is in TokensFile.
	TokenInFile bool      `json:"tokenInFile,omitempty"`
	SignedInAt  time.Time `json:"signedInAt"`
}

// Key is an account's keyring key: one per server and user, so any
// number of connections, to one server or several, each have their own.
func Key(server, userID string) string { return server + "|" + userID }

// Store keeps device tokens in the OS keyring, falling back to a 0600
// file in dir, and the list of accounts in dir (normally state.BaseDir()).
type Store struct {
	dir string
	// noKeyring keeps tokens in TokensFile only (NewFileStore).
	noKeyring bool
	mu        sync.Mutex
}

func NewStore(dir string) *Store { return &Store{dir: dir} }

// NewFileStore keeps tokens only in dir's TokensFile, never in the OS
// keyring: for a throwaway HOME in manual checks, where macOS's
// security tool would still write to the user's own login keychain.
func NewFileStore(dir string) *Store { return &Store{dir: dir, noKeyring: true} }

func (s *Store) keyringSet(key, tok string) error {
	if s.noKeyring {
		return errors.New("keyring disabled")
	}
	return keyring.Set(KeyringService, key, tok)
}

func (s *Store) keyringGet(key string) (string, error) {
	if s.noKeyring {
		return "", keyring.ErrNotFound
	}
	return keyring.Get(KeyringService, key)
}

func (s *Store) keyringDelete(key string) error {
	if s.noKeyring {
		return nil
	}
	return keyring.Delete(KeyringService, key)
}

// Save stores a sign-in's token and lists its account, replacing an
// earlier sign-in of the same account.
func (s *Store) Save(r Result) (Account, error) {
	a := Account{
		Server: r.Server, UserID: r.User.ID, Username: r.User.Username, DisplayName: r.User.DisplayName,
		DeviceID: r.Device.ID, DeviceName: r.Device.Name, SignedInAt: time.Now().UTC().Truncate(time.Second),
	}
	if a.Server == "" || a.UserID == "" || r.Token == "" {
		return Account{}, errors.New("signin: incomplete sign-in result")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(a.Server, a.UserID)
	if err := s.keyringSet(key, r.Token); err != nil {
		slog.Warn("signin: the OS keyring is unavailable; keeping the device token in a file readable only by this user",
			"file", filepath.Join(s.dir, TokensFile), "err", err)
		if err := s.updateTokens(func(m map[string]string) { m[key] = r.Token }); err != nil {
			return Account{}, err
		}
		a.TokenInFile = true
	} else if err := s.updateTokens(func(m map[string]string) { delete(m, key) }); err != nil {
		// A stale copy from an earlier fallback sign-in mustn't linger.
		return Account{}, err
	}
	accounts, err := s.load()
	if err != nil {
		return Account{}, err
	}
	accounts = slices.DeleteFunc(accounts, func(o Account) bool { return o.Server == a.Server && o.UserID == a.UserID })
	if err := s.save(append(accounts, a)); err != nil {
		return Account{}, err
	}
	return a, nil
}

// Token reads an account's token: ErrNoToken if it has none.
func (s *Store) Token(server, userID string) (string, error) {
	key := Key(server, userID)
	tok, err := s.keyringGet(key)
	if err == nil {
		return tok, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tokens, ferr := s.readTokens()
	if ferr != nil {
		return "", ferr
	}
	if tok, ok := tokens[key]; ok {
		return tok, nil
	}
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNoToken
	}
	return "", fmt.Errorf("signin: keyring: %w", err)
}

// TokenSource is an api.TokenSource of an account's token, read once:
// a keyring read on macOS starts a process.
func (s *Store) TokenSource(server, userID string) api.TokenSource {
	return &storedToken{s: s, server: server, userID: userID}
}

type storedToken struct {
	s              *Store
	server, userID string
	mu             sync.Mutex
	tok            string
}

func (t *storedToken) Token(context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tok == "" {
		tok, err := t.s.Token(t.server, t.userID)
		if err != nil {
			return "", err
		}
		t.tok = tok
	}
	return t.tok, nil
}

// Delete forgets an account and its token, locally only (see Logout).
func (s *Store) Delete(server, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := Key(server, userID)
	if err := s.keyringDelete(key); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		// Gone from the list but left in the keyring would be a token
		// nobody can see to remove: say so rather than half-delete.
		if !s.inFile(key) {
			return fmt.Errorf("signin: keyring: %w", err)
		}
	}
	if err := s.updateTokens(func(m map[string]string) { delete(m, key) }); err != nil {
		return err
	}
	accounts, err := s.load()
	if err != nil {
		return err
	}
	return s.save(slices.DeleteFunc(accounts, func(o Account) bool { return o.Server == server && o.UserID == userID }))
}

func (s *Store) inFile(key string) bool {
	tokens, err := s.readTokens()
	_, ok := tokens[key]
	return err == nil && ok
}

// Accounts lists the signed-in accounts in the order they signed in.
func (s *Store) Accounts() ([]Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

// LogoutOptions: Force forgets the account locally even when the server
// can't be told (offline); the device then stays in the user's list on
// the server until removed there.
type LogoutOptions struct {
	HTTPClient *http.Client
	Force      bool
}

// Logout signs the account out: revokes its device token on the server,
// then forgets it here. A token revoked already counts as signed out.
func (s *Store) Logout(ctx context.Context, server, userID string, opts LogoutOptions) error {
	server, err := api.NormalizeServer(server)
	if err != nil {
		return err
	}
	tok, err := s.Token(server, userID)
	if errors.Is(err, ErrNoToken) {
		return s.Delete(server, userID)
	}
	if err != nil {
		return err
	}
	c, err := api.New(server, api.Options{Token: api.StaticToken(tok), HTTPClient: opts.HTTPClient})
	if err != nil {
		return err
	}
	if err := c.SignOut(ctx); err != nil && !errors.Is(err, api.ErrUnauthorized) && !opts.Force {
		return fmt.Errorf("signin: revoke the device on the server (still signed in): %w", err)
	}
	return s.Delete(server, userID)
}

func (s *Store) load() ([]Account, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, AccountsFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Accounts []Account `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("signin: %s: %w", AccountsFile, err)
	}
	return f.Accounts, nil
}

func (s *Store) save(accounts []Account) error {
	if accounts == nil {
		accounts = []Account{}
	}
	return writeJSON(filepath.Join(s.dir, AccountsFile), struct {
		Accounts []Account `json:"accounts"`
	}{accounts})
}

func (s *Store) readTokens() (map[string]string, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, TokensFile))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("signin: %s: %w", TokensFile, err)
	}
	return m, nil
}

// updateTokens changes TokensFile, and removes it once empty: no file
// of secrets where the keyring works.
func (s *Store) updateTokens(change func(map[string]string)) error {
	m, err := s.readTokens()
	if err != nil {
		return err
	}
	n := len(m)
	change(m)
	path := filepath.Join(s.dir, TokensFile)
	if len(m) == 0 {
		if n == 0 {
			return nil
		}
		return os.Remove(path)
	}
	return writeJSON(path, m)
}

// writeJSON replaces path atomically with mode 0600: a crash leaves the
// old file or the new one, and the file is never readable by others,
// not even for a moment.
func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	// CreateTemp makes it 0600 from the start.
	if _, err = tmp.Write(append(raw, '\n')); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
