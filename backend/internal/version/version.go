// Package version holds the build version, stamped at link time.
package version

// Version is overridden by the build via
// -ldflags "-X github.com/do-service/cloud/backend/internal/version.Version=..."
// (see Makefile); "dev" marks a plain `go build`/`go run`.
var Version = "dev"
