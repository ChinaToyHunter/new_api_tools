// Package version holds build-time version information injected via ldflags.
//
// Dockerfile/CI inject:
//
//	-ldflags="-X github.com/new-api-tools/backend/internal/version.GitCommit=<sha>"
//
// Local `go build` without ldflags leaves GitCommit as "dev".
package version

// GitCommit is the full git commit SHA this binary was built from.
var GitCommit = "dev"

// ReleaseURL is the GitHub repo the update checker compares against.
const ReleaseURL = "https://github.com/ChinaToyHunter/new_api_tools"
