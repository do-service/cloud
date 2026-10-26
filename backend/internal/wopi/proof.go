package wopi

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Proof keys (WOPI "Verifying that requests originate from Office for the
// web"): the editor signs each request with the key its discovery
// publishes, so a stolen access token alone can't be used from elsewhere.

// maxProofAge is how old a signed request may be.
const maxProofAge = 20 * time.Minute

var errBadProof = errors.New("wopi: request not signed by the editor")

// ticksAtUnixEpoch is .NET's DateTime.Ticks of 1970-01-01: the timestamp
// counts 100 ns ticks from year 1.
const ticksAtUnixEpoch = 621355968000000000

// proofData is what the editor signs: the access token, the request URL in
// upper case and the timestamp, each preceded by its length.
func proofData(token, url string, ticks int64) []byte {
	var b []byte
	b = binary.BigEndian.AppendUint32(b, uint32(len(token)))
	b = append(b, token...)
	url = strings.ToUpper(url)
	b = binary.BigEndian.AppendUint32(b, uint32(len(url)))
	b = append(b, url...)
	b = binary.BigEndian.AppendUint32(b, 8)
	return binary.BigEndian.AppendUint64(b, uint64(ticks))
}

// verifyProof checks a request's X-WOPI-Proof headers: signed by the
// current key, or across a rotation by the old one, and recent.
func verifyProof(e *Editor, token, url, timestamp, proof, oldProof string, now time.Time) error {
	if e.ProofKey == nil {
		return nil
	}
	ticks, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return errBadProof
	}
	signed := time.Unix(0, (ticks-ticksAtUnixEpoch)*100)
	if now.Sub(signed) > maxProofAge || signed.Sub(now) > maxProofAge {
		return errBadProof
	}
	digest := sha256.Sum256(proofData(token, url, ticks))
	ok := func(key *rsa.PublicKey, sig string) bool {
		if key == nil || sig == "" {
			return false
		}
		raw, err := base64.StdEncoding.DecodeString(sig)
		return err == nil && rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], raw) == nil
	}
	if ok(e.ProofKey, proof) || ok(e.ProofKey, oldProof) || ok(e.OldProofKey, proof) {
		return nil
	}
	return errBadProof
}
