package app

import (
	"errors"
	"testing"
)

func TestNormalizeInput(t *testing.T) {
	for in, want := range map[string]string{
		"cloud.example.com":       "https://cloud.example.com",
		" cloud.example.com:8443": "https://cloud.example.com:8443",
		"localhost:8090":          "http://localhost:8090",
		"127.0.0.1:8090":          "http://127.0.0.1:8090",
		"[::1]:8090":              "http://[::1]:8090",
		"https://127.0.0.1:8090":  "https://127.0.0.1:8090",
		"http://localhost:8090/":  "http://localhost:8090",
	} {
		got, err := normalizeInput(in)
		if err != nil || got != want {
			t.Errorf("normalizeInput(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "http://cloud.example.com", "ftp://cloud.example.com"} {
		if _, err := normalizeInput(in); !errors.Is(err, ErrInvalidServer) {
			t.Errorf("normalizeInput(%q): err = %v, want ErrInvalidServer", in, err)
		}
	}
}
