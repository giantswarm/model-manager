package main

import (
	"github.com/giantswarm/model-manager/cmd"
	"github.com/giantswarm/model-manager/internal/buildinfo"
)

// Set by the build via ldflags (-X main.version=...); what a build leaves at
// these defaults is resolved from the Go toolchain's build info (the tag at
// HEAD, the commit, its time), so a tagged build reports its release without
// a build flag.
var (
	version = buildinfo.DevVersion
	commit  = buildinfo.UnknownCommit
	date    = buildinfo.UnknownDate
)

func main() {
	cmd.SetBuild(buildinfo.Read(version, commit, date))
	cmd.Execute()
}
