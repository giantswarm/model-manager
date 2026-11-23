package main

import (
	"github.com/giantswarm/model-manager/cmd"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/pkg/project"
)

func main() {
	cmd.SetBuild(buildinfo.Read(project.Version(), project.GitSHA(), project.BuildTimestamp()))
	cmd.Execute()
}
