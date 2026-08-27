package cmd

// devVersion is what a build without -ldflags reports; release.sh uses the same default
// so a local build and an untagged CI build stay indistinguishable from each other.
const devVersion = "v0.0.0"

// Injected at build time via -ldflags -X (see release.sh).
var (
	Version        = devVersion
	CommitHash     = "0000000"
	BuildTimestamp = "1970-01-01T00:00:00Z"
	Builder        = "unknown"
)
