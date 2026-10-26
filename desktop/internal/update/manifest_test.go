package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"
)

func TestAssetPlatform(t *testing.T) {
	for name, want := range map[string][3]string{
		"Cloud-1.2.3-macos-universal.app.tar.gz":   {"darwin", "", "1.2.3"},
		"Cloud-1.2.3-rc1-windows-amd64-update.zip": {"windows", "amd64", "1.2.3-rc1"},
		"Cloud-1.2.3-windows-arm64-update.zip":     {"windows", "arm64", "1.2.3"},
		"Cloud-1.2.3-linux-amd64.AppImage":         {"linux", "amd64", "1.2.3"},
		"Cloud-1.2.3-linux-arm64.AppImage":         {"linux", "arm64", "1.2.3"},
	} {
		p, a, v, ok := AssetPlatform(name)
		if !ok || [3]string{p, a, v} != want {
			t.Errorf("%s: %s %s %s %v", name, p, a, v, ok)
		}
	}
	for _, name := range []string{
		"Cloud-1.2.3-macos-universal.dmg", "Cloud-1.2.3-windows-amd64-setup.exe", "Cloud-1.2.3-windows-amd64.zip",
		"Cloud-1.2.3-linux-amd64.AppImage.sha256", "cloud-desktop_1.2.3_amd64.deb", "update.json",
	} {
		if _, _, _, ok := AssetPlatform(name); ok {
			t.Errorf("%s taken for an update asset", name)
		}
	}
}

// keyPair is a throwaway signing key, as PEM the way the secret and
// update-public-key.pem hold them.
func keyPair(t *testing.T) (privPEM, pubPEM []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p8, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8}), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pk})
}

func TestReleaseKey(t *testing.T) {
	if _, err := ParsePublicKey(ReleaseKey()); err != nil {
		t.Fatalf("the compiled-in key: %v", err)
	}
}

// fakeHost is enough of the app for Wails' updater without a window.
type fakeHost struct{}

func (fakeHost) Emit(string, ...any) bool                              { return false }
func (fakeHost) OnEvent(string, func(any)) func()                      { return func() {} }
func (fakeHost) OpenWindow(updater.WindowOptions) updater.WindowHandle { return nil }
func (fakeHost) Quit()                                                 {}

// release serves a release's assets and its manifest as GitHub would.
type release struct {
	srv   *httptest.Server
	files map[string][]byte
}

func newRelease(t *testing.T) *release {
	r := &release{files: map[string][]byte{}}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, ok := r.files[strings.TrimPrefix(req.URL.Path, "/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

// publish signs the files (name → content) with privPEM as the release
// job does and serves them with the manifest.
func (r *release) publish(t *testing.T, privPEM, pubPEM []byte, version string, files map[string][]byte) Manifest {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
		r.files[name] = data
	}
	priv, err := ParsePrivateKey(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	m, err := SignRelease(priv, pub, version, r.srv.URL, paths, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.files[ManifestName], _ = json.Marshal(m)
	return m
}

// install runs Wails' updater as the app does — the endpoint provider
// over the manifest, the key pinned — up to the staged download, for a
// client of version 1.0.0 on linux/amd64.
func (r *release) install(t *testing.T, pubPEM []byte, platform, arch string) (string, error) {
	t.Helper()
	prov, err := NewProvider(Source{ManifestURL: r.srv.URL + "/" + ManifestName})
	if err != nil {
		t.Fatal(err)
	}
	u := updater.New(fakeHost{})
	if err := u.Init(updater.Config{CurrentVersion: "1.0.0", Providers: []updater.Provider{prov}, PublicKey: pubPEM, Platform: platform, Arch: arch, Window: updater.WindowNone}); err != nil {
		t.Fatal(err)
	}
	rel, err := u.Check(context.Background())
	if err != nil {
		return "", err
	}
	if rel == nil {
		return "", nil
	}
	err = u.DownloadAndInstall(context.Background())
	if p := u.DownloadedPath(); p != "" {
		// Staged beside the test binary, in a wails-update-* dir.
		t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(p)) })
		return p, err
	}
	return "", err
}

func TestSignedReleaseInstalls(t *testing.T) {
	privPEM, pubPEM := keyPair(t)
	r := newRelease(t)
	appImage := bytes.Repeat([]byte("new AppImage "), 1000)
	m := r.publish(t, privPEM, pubPEM, "1.1.0", map[string][]byte{
		"Cloud-1.1.0-linux-amd64.AppImage":       appImage,
		"Cloud-1.1.0-linux-arm64.AppImage":       []byte("arm"),
		"Cloud-1.1.0-macos-universal.app.tar.gz": appTarGz(t, "Облако.app"),
		"Cloud-1.1.0-macos-universal.dmg":        []byte("not an update asset"),
	})
	if len(m.Artifacts) != 3 {
		t.Fatalf("manifest: %+v", m)
	}

	got, err := r.install(t, pubPEM, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(got); !bytes.Equal(data, appImage) || filepath.Base(got) != "Cloud-1.1.0-linux-amd64.AppImage" {
		t.Fatalf("staged %s", got)
	}

	// The .app comes out of its archive, ready to be renamed over the old.
	got, err = r.install(t, pubPEM, "darwin", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got) != "Облако.app" {
		t.Fatalf("staged %s", got)
	}
	// Windows keeps no exec bit.
	if fi, err := os.Stat(filepath.Join(got, "Contents", "MacOS", "cloud-desktop")); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm()&0o100 == 0) {
		t.Fatalf("the app's binary: %v %v", fi, err)
	}

	// Up to date: a client of the release's own version finds nothing.
	prov, _ := NewProvider(Source{ManifestURL: r.srv.URL + "/" + ManifestName})
	u := updater.New(fakeHost{})
	_ = u.Init(updater.Config{CurrentVersion: "1.1.0", Providers: []updater.Provider{prov}, PublicKey: pubPEM, Window: updater.WindowNone})
	if rel, err := u.Check(context.Background()); rel != nil || err != nil {
		t.Fatalf("same version: %+v %v", rel, err)
	}
}

func TestTamperedReleaseRefused(t *testing.T) {
	privPEM, pubPEM := keyPair(t)
	const name = "Cloud-1.1.0-linux-amd64.AppImage"

	// The asset changed after signing.
	r := newRelease(t)
	r.publish(t, privPEM, pubPEM, "1.1.0", map[string][]byte{name: []byte("signed bytes")})
	r.files[name] = []byte("evil bytes!!")
	if _, err := r.install(t, pubPEM, "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("tampered asset: %v", err)
	}

	// Signed by another key: the digest matches, the signature doesn't.
	otherPriv, otherPub := keyPair(t)
	r = newRelease(t)
	r.publish(t, otherPriv, otherPub, "1.1.0", map[string][]byte{name: []byte("signed bytes")})
	if _, err := r.install(t, pubPEM, "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "did not verify") {
		t.Fatalf("another key: %v", err)
	}

	// The signature stripped from the manifest: refused, not installed
	// on the digest alone.
	r = newRelease(t)
	m := r.publish(t, privPEM, pubPEM, "1.1.0", map[string][]byte{name: []byte("signed bytes")})
	m.Artifacts[0].Signature, m.Artifacts[0].SignatureAlgo = "", ""
	r.files[ManifestName], _ = json.Marshal(m)
	if _, err := r.install(t, pubPEM, "linux", "amd64"); err == nil || !strings.Contains(err.Error(), "must carry a signature") {
		t.Fatalf("no signature: %v", err)
	}
}

func TestSignReleaseRefuses(t *testing.T) {
	privPEM, pubPEM := keyPair(t)
	_, otherPub := keyPair(t)
	priv, _ := ParsePrivateKey(privPEM)
	pub, _ := ParsePublicKey(pubPEM)
	wrong, _ := ParsePublicKey(otherPub)
	dir := t.TempDir()
	file := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	img := file("Cloud-1.1.0-linux-amd64.AppImage")
	if _, err := SignRelease(priv, wrong, "1.1.0", "https://x", []string{img}, time.Now()); err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Fatalf("a key that isn't the clients': %v", err)
	}
	if _, err := SignRelease(priv, pub, "1.2.0", "https://x", []string{img}, time.Now()); err == nil {
		t.Fatal("signed another version's asset")
	}
	if _, err := SignRelease(priv, pub, "1.1.0", "https://x", []string{file("Cloud-1.1.0-macos-universal.dmg")}, time.Now()); err == nil {
		t.Fatal("a manifest without update assets")
	}
	if _, err := ParsePrivateKey(pubPEM); err == nil {
		t.Fatal("a public key taken for the private one")
	}
}

// appTarGz is a minimal .app in a tar.gz, as `make app-update` packs it.
func appTarGz(t *testing.T, name string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(path string, mode int64, data string) {
		hdr := &tar.Header{Name: path, Mode: mode, Size: int64(len(data)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(path, "/") {
			hdr.Typeflag, hdr.Size = tar.TypeDir, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	add(name+"/", 0o755, "")
	add(name+"/Contents/", 0o755, "")
	add(name+"/Contents/Info.plist", 0o644, `<?xml version="1.0"?><plist version="1.0"><dict><key>CFBundleExecutable</key><string>cloud-desktop</string></dict></plist>`)
	add(name+"/Contents/MacOS/", 0o755, "")
	add(name+"/Contents/MacOS/cloud-desktop", 0o755, "binary")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
