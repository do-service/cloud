package main

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/do-service/cloud/desktop/internal/update"
)

func TestSignUpdate(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p8, _ := x509.MarshalPKCS8PrivateKey(priv)
	pk, _ := x509.MarshalPKIXPublicKey(pub)
	dir := t.TempDir()
	pubFile := filepath.Join(dir, "pub.pem")
	if err := os.WriteFile(pubFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pk}), 0o644); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "Cloud-2.0.0-linux-amd64.AppImage")
	if err := os.WriteFile(img, []byte("image"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "Cloud-2.0.0-macos-universal.dmg")
	if err := os.WriteFile(other, []byte("dmg"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "update.json")
	args := []string{"-version", "2.0.0", "-base", "https://example.com/v2.0.0/", "-o", out, "-public-key", pubFile, img, other}

	t.Setenv("UPDATE_SIGNING_KEY", "")
	if err := run(args); err == nil || !strings.Contains(err.Error(), "UPDATE_SIGNING_KEY") {
		t.Fatalf("without the key: %v", err)
	}
	t.Setenv("UPDATE_SIGNING_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: p8})))
	if err := run(append(append([]string{}, args[:8]...), other)); err == nil || !strings.Contains(err.Error(), "no update assets") {
		t.Fatalf("no update assets: %v", err)
	}
	// The clients' own key isn't this one: refused.
	noPub := append(append([]string{}, args[:6]...), img)
	if err := run(noPub); err == nil || !strings.Contains(err.Error(), "doesn't match") {
		t.Fatalf("against the compiled-in key: %v", err)
	}

	if err := run(args); err != nil {
		t.Fatal(err)
	}
	var m update.Manifest
	data, _ := os.ReadFile(out)
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != "2.0.0" || len(m.Artifacts) != 1 {
		t.Fatalf("manifest: %s", data)
	}
	a := m.Artifacts[0]
	if a.URL != "https://example.com/v2.0.0/Cloud-2.0.0-linux-amd64.AppImage" || a.Platform != "linux" || a.Arch != "amd64" || a.Size != 5 || a.SignatureAlgo != "ed25519ph" {
		t.Fatalf("artifact: %+v", a)
	}
	sum := sha512.Sum512([]byte("image"))
	sig, _ := base64.StdEncoding.DecodeString(a.Signature)
	if err := ed25519.VerifyWithOptions(pub, sum[:], sig, &ed25519.Options{Hash: crypto.SHA512}); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if strings.Contains(string(data), "PRIVATE") {
		t.Fatal("the key is in the manifest")
	}
}
