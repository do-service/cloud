package state_test

import (
	"testing"

	"github.com/do-service/cloud/desktop/internal/api"
	"github.com/do-service/cloud/desktop/internal/state"
)

// The registry and the keyring key one connection by the server address
// as state and api normalize it; written separately, they must agree, or
// a connection's token would be looked up under another spelling.
func TestNormalizeServerAgreesWithAPI(t *testing.T) {
	for _, in := range []string{
		"https://Cloud.Example.RU/", "HTTPS://cloud.example.ru:443", "https://cloud.example.ru:8443/x/",
		"http://localhost:8090", "http://LOCALHOST:80/", "http://127.0.0.1:80/", "http://[::1]:8090",
		"https://[2001:DB8::1]", " https://cloud.example.ru ", "https://cloud.example.ru/a%20b/",
		"", "cloud.example.ru", "ftp://x", "http://cloud.example.ru", "https://u:p@x", "https://x/?a=1",
		"https://x/#f", "http://10.0.0.1:8090", "https://xn--80aswg.xn--p1ai",
	} {
		s, serr := state.NormalizeServer(in)
		a, aerr := api.NormalizeServer(in)
		if s != a || (serr == nil) != (aerr == nil) {
			t.Errorf("%q: state %q (%v), api %q (%v)", in, s, serr, a, aerr)
		}
	}
}
