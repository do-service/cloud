package update

import (
	"net"
	"net/http"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/endpoint"
)

// Repository is where releases are published.
const Repository = "do-service/cloud"

// ManifestURL is the update manifest (cmd/signupdate) of the latest
// release that isn't a pre-release: GitHub redirects
// releases/latest/download/<asset> to that release's asset. Not the
// releases API, which allows 60 requests an hour per IP without a token —
// an office behind one address would run out.
const ManifestURL = "https://github.com/" + Repository + "/releases/latest/download/" + ManifestName

// ManifestName is the manifest's asset name in a release.
const ManifestName = "update.json"

// ReleasePage is the release's page on GitHub: notes, and the packages
// for whoever updates by hand.
func ReleasePage(version string) string {
	return "https://github.com/" + Repository + "/releases/tag/v" + version
}

// Source is where releases come from and the key they are verified with.
type Source struct {
	ManifestURL string
	PublicKey   []byte
}

// NewProvider makes the updater's provider for src: Wails' endpoint
// provider over the manifest, which carries each artifact's ed25519ph
// signature (its GitHub provider reads only checksum files, so with a
// pinned key it would fail every release).
func NewProvider(src Source) (updater.Provider, error) {
	// No overall timeout, unlike the provider's default client (30 s):
	// the download is tens of megabytes. Stalls end on the header timeout
	// or with the context.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 15 * time.Second
	t.ResponseHeaderTimeout = 30 * time.Second
	return endpoint.New(endpoint.Config{URL: src.ManifestURL, HTTPClient: &http.Client{Transport: t}})
}
