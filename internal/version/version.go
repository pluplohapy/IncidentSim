// Package version exposes build information shared by the CLI and the HTTP API.
package version

// Version is the semantic version of the build. It is overridden at link time
// with -ldflags "-X incidentsim/internal/version.Version=vX.Y.Z".
var Version = "0.0.0-dev"

// Commit is the git revision the binary was built from. Like Version it is
// meant to be injected by the release pipeline.
var Commit = "unknown"

// String returns a human readable build identifier.
func String() string {
	if Commit == "" || Commit == "unknown" {
		return Version
	}
	return Version + " (" + Commit + ")"
}
