package update

import (
	"crypto"
	"crypto/ed25519"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// SignatureAlgo is how releases are signed: Ed25519ph over the SHA-512 of
// the artifact, as Wails' updater checks it ("ed25519ph": it hashes the
// download with SHA-512 as it streams and verifies the signature over
// that digest under Config.PublicKey).
const SignatureAlgo = "ed25519ph"

// Manifest is Wails' update manifest (schemaVersion 1) as its endpoint
// provider reads it.
type Manifest struct {
	SchemaVersion int        `json:"schemaVersion"`
	Version       string     `json:"version"`
	Name          string     `json:"name,omitempty"`
	PublishedAt   string     `json:"publishedAt,omitempty"`
	Artifacts     []Artifact `json:"artifacts"`
}

// Artifact is one platform's update in the manifest. Digest and
// Signature are base64.
type Artifact struct {
	URL           string `json:"url"`
	Filename      string `json:"filename"`
	Size          int64  `json:"size"`
	Platform      string `json:"platform"`
	Arch          string `json:"arch,omitempty"`
	DigestAlgo    string `json:"digestAlgo"`
	Digest        string `json:"digest"`
	SignatureAlgo string `json:"signatureAlgo"`
	Signature     string `json:"signature"`
}

// The update assets of a release, as the Makefile names them:
// the .app of both architectures, Windows' cloud-desktop.exe alone (the
// updater replaces that one file), the AppImage.
var updateAssets = []struct {
	re       *regexp.Regexp
	platform string
}{
	{regexp.MustCompile(`^Cloud-(.+)-macos-universal\.app\.tar\.gz$`), "darwin"},
	{regexp.MustCompile(`^Cloud-(.+)-windows-(amd64|arm64)-update\.zip$`), "windows"},
	{regexp.MustCompile(`^Cloud-(.+)-linux-(amd64|arm64)\.AppImage$`), "linux"},
}

// AssetPlatform tells which platform and architecture a release asset
// updates ("" arch: any), and its version; ok is false for other assets.
func AssetPlatform(name string) (platform, arch, version string, ok bool) {
	for _, a := range updateAssets {
		if m := a.re.FindStringSubmatch(name); m != nil {
			if len(m) > 2 {
				arch = m[2]
			}
			return a.platform, arch, m[1], true
		}
	}
	return "", "", "", false
}

// ParsePrivateKey reads an ed25519 private key in PEM (PKCS#8), the form
// of the UPDATE_SIGNING_KEY secret.
func ParsePrivateKey(pemBytes []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("update: no PEM block in the private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("update: the private key isn't PKCS#8")
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("update: the private key is %T, not ed25519", k)
	}
	return priv, nil
}

// SignRelease makes the manifest of version from the update assets among
// files (others are skipped): each signed with priv, its URL baseURL/name.
// Every signature is checked against pub before the manifest is
// returned, so a secret that doesn't match the compiled-in key fails the
// release instead of every client's update.
func SignRelease(priv ed25519.PrivateKey, pub ed25519.PublicKey, version, baseURL string, files []string, now time.Time) (Manifest, error) {
	m := Manifest{SchemaVersion: 1, Version: version, Name: "v" + version, PublishedAt: now.UTC().Format(time.RFC3339)}
	seen := map[string]string{}
	for _, f := range files {
		name := filepath.Base(f)
		platform, arch, v, ok := AssetPlatform(name)
		if !ok {
			continue
		}
		if v != version {
			return Manifest{}, fmt.Errorf("update: %s is of version %s, not %s", name, v, version)
		}
		key := platform + "/" + arch
		if prev, dup := seen[key]; dup {
			return Manifest{}, fmt.Errorf("update: both %s and %s update %s", prev, name, key)
		}
		seen[key] = name
		digest, size, err := sha512File(f)
		if err != nil {
			return Manifest{}, err
		}
		sig, err := priv.Sign(nil, digest, &ed25519.Options{Hash: crypto.SHA512})
		if err != nil {
			return Manifest{}, err
		}
		if err := ed25519.VerifyWithOptions(pub, digest, sig, &ed25519.Options{Hash: crypto.SHA512}); err != nil {
			return Manifest{}, errors.New("update: the signing key doesn't match the public key the clients have")
		}
		m.Artifacts = append(m.Artifacts, Artifact{
			URL: strings.TrimRight(baseURL, "/") + "/" + name, Filename: name, Size: size,
			Platform: platform, Arch: arch,
			DigestAlgo: "sha512", Digest: base64.StdEncoding.EncodeToString(digest),
			SignatureAlgo: SignatureAlgo, Signature: base64.StdEncoding.EncodeToString(sig),
		})
	}
	if len(m.Artifacts) == 0 {
		return Manifest{}, errors.New("update: no update assets among the files")
	}
	return m, nil
}

func sha512File(name string) ([]byte, int64, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	h := sha512.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return nil, 0, err
	}
	return h.Sum(nil), n, nil
}
