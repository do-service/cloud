package wopi

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// discoveryTTL is how long the editor's discovery is trusted: its URLs
// change with its version, its proof key when it is rotated.
const discoveryTTL = time.Hour

// refetchInterval limits Refetch: a bad proof may mean a rotated key, but
// anyone can send one, and each must not cost the editor a discovery.
const refetchInterval = 30 * time.Second

// Editor is what the discovery says about the editor: which file types it
// opens and where, and the keys its requests are signed with.
type Editor struct {
	// Actions maps a lower-case extension to the editor URL (without
	// WOPISrc) that edits it, or failing that views it.
	Actions map[string]Action
	// ProofKey and OldProofKey verify X-WOPI-Proof; nil when the editor
	// doesn't sign (then nothing is verified).
	ProofKey, OldProofKey *rsa.PublicKey
	// Origin is where the browser loads the editor from
	// (scheme://host[:port]), what the app's CSP must allow.
	Origin string
}

// Action is how a file type opens in the editor.
type Action struct {
	URL string
	// Edit: the editor can change it; otherwise it only shows it.
	Edit bool
}

// Discovery fetches and caches the editor's discovery document.
type Discovery struct {
	url string
	// editorURL replaces the origin of the editor URLs in the discovery
	// when set: an editor builds them from the address it was asked at,
	// and the server may reach it at one the browser can't.
	editorURL string
	client    *http.Client

	// refetchEvery is refetchInterval, shorter in tests.
	refetchEvery time.Duration

	mu      sync.Mutex
	editor  *Editor
	fetched time.Time
	// refetched is the last Refetch that went to the editor, whatever came
	// of it.
	refetched time.Time
}

// NewDiscovery reads the discovery at url; editorURL is the editor's
// origin as the browser reaches it, "" to take the discovery's.
func NewDiscovery(url, editorURL string) *Discovery {
	return &Discovery{url: url, editorURL: editorURL, client: &http.Client{Timeout: 10 * time.Second}, refetchEvery: refetchInterval}
}

// Origin is the editor's origin from the discovery fetched so far, "" if
// none yet. It never fetches: the CSP asks on every response.
func (d *Discovery) Origin() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.editor == nil {
		return ""
	}
	return d.editor.Origin
}

// Warm fetches the discovery in the background until it succeeds or ctx
// ends, so the editor's origin is in the CSP before anyone opens a
// document: a page served without it can't frame the editor.
func (d *Discovery) Warm(ctx context.Context) {
	for {
		if _, err := d.Editor(ctx); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// Editor returns the cached discovery, fetching it when stale. If the
// editor can't be reached, a stale copy is better than none.
func (d *Discovery) Editor(ctx context.Context) (*Editor, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.editor != nil && time.Since(d.fetched) < discoveryTTL {
		return d.editor, nil
	}
	e, err := d.fetch(ctx)
	if err != nil {
		if d.editor != nil {
			slog.WarnContext(ctx, "wopi: discovery unavailable, using the last one", "err", err)
			return d.editor, nil
		}
		return nil, err
	}
	d.editor, d.fetched = e, time.Now()
	return e, nil
}

// Refetch reads the discovery again before its TTL, for when a request's
// proof fails: the editor may have rotated its key. It goes to the editor
// at most once per refetchEvery; otherwise, or if the editor can't be
// reached, it returns what is cached. Callers waiting on the lock get the
// result of the one fetch in flight.
func (d *Discovery) Refetch(ctx context.Context) *Editor {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.editor != nil && (time.Since(d.fetched) < d.refetchEvery || time.Since(d.refetched) < d.refetchEvery) {
		return d.editor
	}
	d.refetched = time.Now()
	e, err := d.fetch(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wopi: discovery refetch failed", "err", err)
		return d.editor
	}
	d.editor, d.fetched = e, time.Now()
	return e
}

type discoveryXML struct {
	Zones []struct {
		Name string `xml:"name,attr"`
		Apps []struct {
			Actions []struct {
				Name   string `xml:"name,attr"`
				Ext    string `xml:"ext,attr"`
				URLSrc string `xml:"urlsrc,attr"`
			} `xml:"action"`
		} `xml:"app"`
	} `xml:"net-zone"`
	ProofKey struct {
		Modulus     string `xml:"modulus,attr"`
		Exponent    string `xml:"exponent,attr"`
		OldModulus  string `xml:"oldmodulus,attr"`
		OldExponent string `xml:"oldexponent,attr"`
	} `xml:"proof-key"`
}

// placeholders are urlsrc's optional parameters (<ui=UI_LLCC&>), none of
// which this host fills in.
var placeholders = regexp.MustCompile(`<[^>]*>`)

func (d *Discovery) fetch(ctx context.Context) (*Editor, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
	if err != nil {
		return nil, err
	}
	res, err := d.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wopi: discovery: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wopi: discovery: status %d", res.StatusCode)
	}
	return parseDiscovery(io.LimitReader(res.Body, 4<<20), d.editorURL)
}

// origin is a URL's scheme://host[:port], "" if it has none.
func origin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func parseDiscovery(r io.Reader, editorURL string) (*Editor, error) {
	var doc discoveryXML
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("wopi: discovery: %w", err)
	}
	e := &Editor{Actions: map[string]Action{}}
	for _, z := range doc.Zones {
		for _, app := range z.Apps {
			for _, a := range app.Actions {
				ext := strings.ToLower(a.Ext)
				// view_comment is Collabora's for PDF: shown, annotated.
				view := a.Name == "view" || a.Name == "view_comment"
				if ext == "" || a.URLSrc == "" || a.Name != "edit" && !view {
					continue
				}
				if cur, ok := e.Actions[ext]; ok && (cur.Edit || view) {
					continue
				}
				src, o := placeholders.ReplaceAllString(a.URLSrc, ""), origin(a.URLSrc)
				if editorURL != "" && o != "" {
					src, o = editorURL+strings.TrimPrefix(src, o), editorURL
				}
				e.Actions[ext] = Action{URL: src, Edit: a.Name == "edit"}
				if e.Origin == "" {
					e.Origin = o
				}
			}
		}
	}
	if len(e.Actions) == 0 {
		return nil, errors.New("wopi: discovery lists no file types")
	}
	var err error
	if e.ProofKey, err = rsaKey(doc.ProofKey.Modulus, doc.ProofKey.Exponent); err != nil {
		return nil, err
	}
	if e.OldProofKey, err = rsaKey(doc.ProofKey.OldModulus, doc.ProofKey.OldExponent); err != nil {
		return nil, err
	}
	return e, nil
}

// rsaKey builds a public key from discovery's base64 modulus and exponent;
// nil when there is none.
func rsaKey(modulus, exponent string) (*rsa.PublicKey, error) {
	if modulus == "" || exponent == "" {
		return nil, nil
	}
	m, err := base64.StdEncoding.DecodeString(modulus)
	if err != nil {
		return nil, fmt.Errorf("wopi: proof key modulus: %w", err)
	}
	ex, err := base64.StdEncoding.DecodeString(exponent)
	if err != nil {
		return nil, fmt.Errorf("wopi: proof key exponent: %w", err)
	}
	e := new(big.Int).SetBytes(ex)
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
		return nil, errors.New("wopi: proof key exponent out of range")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(m), E: int(e.Int64())}, nil
}
