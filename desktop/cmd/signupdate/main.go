// Command signupdate signs a release's update assets and writes the
// update manifest the desktop client checks (internal/update):
//
//	UPDATE_SIGNING_KEY=<PEM> signupdate -version 1.2.3 \
//	    -base https://github.com/do-service/cloud/releases/download/v1.2.3 \
//	    -o dist/update.json dist/*
//
// The key (ed25519, PEM PKCS#8) comes only from the environment, never a
// flag or a file, and is never printed. Each signature is checked against
// the public key the clients have compiled in (or -public-key, for a test
// release signed with a throwaway key) before the manifest is written.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/do-service/cloud/desktop/internal/update"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "signupdate:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("signupdate", flag.ContinueOnError)
	version := fs.String("version", "", "the release's version, without v")
	base := fs.String("base", "", "the URL the assets are downloaded from (…/releases/download/v<version>)")
	out := fs.String("o", update.ManifestName, "the manifest to write")
	pubFile := fs.String("public-key", "", "PEM of the public key to check against (default: the clients' own)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *version == "" || *base == "" || fs.NArg() == 0 {
		return fmt.Errorf("usage: signupdate -version V -base URL [-o FILE] [-public-key PEM] ASSET…")
	}
	secret := os.Getenv("UPDATE_SIGNING_KEY")
	if secret == "" {
		return fmt.Errorf("UPDATE_SIGNING_KEY is not set")
	}
	priv, err := update.ParsePrivateKey([]byte(secret))
	if err != nil {
		return err
	}
	pubPEM := update.ReleaseKey()
	if *pubFile != "" {
		if pubPEM, err = os.ReadFile(*pubFile); err != nil {
			return err
		}
	}
	pub, err := update.ParsePublicKey(pubPEM)
	if err != nil {
		return err
	}
	m, err := update.SignRelease(priv, pub, *version, *base, fs.Args(), time.Now())
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil {
		return err
	}
	for _, a := range m.Artifacts {
		arch := a.Arch
		if arch == "" {
			arch = "any"
		}
		fmt.Printf("signed %s (%s/%s, %d bytes)\n", a.Filename, a.Platform, arch, a.Size)
	}
	return nil
}
