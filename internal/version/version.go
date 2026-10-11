// Package version holds build information, set with -ldflags at release time.
package version

var (
	Version = "0.0.0-dev"
	Commit  = ""
)

// APIVersion is the REST API major version served under /v1.
const APIVersion = "v1"
