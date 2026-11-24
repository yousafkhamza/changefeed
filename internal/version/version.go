// Package version holds build information set by -ldflags at release time.
package version

// Set with: -ldflags "-X github.com/yousafkhamza/changefeed/internal/version.Version=v0.1.0 ..."
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)
