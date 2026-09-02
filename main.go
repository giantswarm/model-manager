package main

import "github.com/giantswarm/model-manager/cmd"

// Set by the build via ldflags (-X main.version=...).
var (
	version = "dev"
	commit  = "unknown"
	date    = "unknown"
)

func main() {
	cmd.SetVersion(version)
	cmd.SetBuildInfo(commit, date)
	cmd.Execute()
}
