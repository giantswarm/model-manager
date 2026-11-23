// Package project holds the build identity the release pipeline stamps. The
// architect orb's go-build job and the generated Makefile pass
// -X <module>/pkg/project.version|gitSHA|buildTimestamp; a tag build stamps
// the tag without its v. A variable a build leaves at its default is resolved
// from the Go toolchain's build info (internal/buildinfo).
package project

import "github.com/giantswarm/model-manager/internal/buildinfo"

var (
	version        = buildinfo.DevVersion
	gitSHA         = buildinfo.UnknownCommit
	buildTimestamp = buildinfo.UnknownDate
)

// Version is the stamped release version, buildinfo.DevVersion when unset.
func Version() string { return version }

// GitSHA is the stamped commit, buildinfo.UnknownCommit when unset.
func GitSHA() string { return gitSHA }

// BuildTimestamp is the stamped build time, buildinfo.UnknownDate when unset.
func BuildTimestamp() string { return buildTimestamp }
