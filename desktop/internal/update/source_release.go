//go:build !updatetest

package update

// DefaultSource is the GitHub release and the compiled-in key. Builds
// with the updatetest tag may point elsewhere (source_updatetest.go);
// release builds can't.
func DefaultSource() Source {
	return Source{ManifestURL: ManifestURL, PublicKey: ReleaseKey()}
}
