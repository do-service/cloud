//go:build updatetest

package update

import (
	"log/slog"
	"os"
)

// DefaultSource of an updatetest build, for checking a whole update on a
// machine against a fake release signed with a throwaway key:
// CLOUD_UPDATE_MANIFEST is the manifest's URL, CLOUD_UPDATE_PUBLIC_KEY
// the PEM file of that key. Release builds don't have the tag, so these
// variables mean nothing to them.
func DefaultSource() Source {
	src := Source{ManifestURL: ManifestURL, PublicKey: ReleaseKey()}
	if u := os.Getenv("CLOUD_UPDATE_MANIFEST"); u != "" {
		src.ManifestURL = u
	}
	if p := os.Getenv("CLOUD_UPDATE_PUBLIC_KEY"); p != "" {
		key, err := os.ReadFile(p)
		if err != nil {
			slog.Error("update: CLOUD_UPDATE_PUBLIC_KEY", "err", err)
			// Never no key: one that matches nothing fails every release.
			key = []byte("unreadable")
		}
		src.PublicKey = key
	}
	slog.Warn("update: an updatetest build", "manifest", src.ManifestURL)
	return src
}
