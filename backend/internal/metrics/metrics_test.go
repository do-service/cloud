package metrics

import "testing"

func TestRouteLabel(t *testing.T) {
	cases := map[string]string{
		"":                    UnmatchedRoute,
		"GET /healthz":        "/healthz",
		"/":                   "/",
		"GET /api/v1/f/{id}":  "/api/v1/f/{id}",
		"example.com/static/": "example.com/static/",
	}
	for pattern, want := range cases {
		if got := RouteLabel(pattern); got != want {
			t.Errorf("RouteLabel(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func TestStatusClass(t *testing.T) {
	cases := map[int]string{200: "2xx", 204: "2xx", 301: "3xx", 404: "4xx", 503: "5xx", 0: "unknown", 600: "unknown", 101: "1xx"}
	for status, want := range cases {
		if got := StatusClass(status); got != want {
			t.Errorf("StatusClass(%d) = %q, want %q", status, got, want)
		}
	}
}
