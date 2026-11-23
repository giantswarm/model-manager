// Package buildinfo resolves the version model-manager reports: what the
// build stamped into pkg/project with -ldflags -X when it did (the release
// pipeline stamps the tag), else what the Go toolchain recorded from version
// control — the module version, the commit and its time. The module path
// carries no major-version suffix, so the toolchain ignores v2+ tags and its
// module version is a v1 pseudo-version: only the stamp names a v2 release.
package buildinfo

import (
	"runtime/debug"
	"strings"
)

// The pkg/project defaults a build without -X leaves in place.
const (
	DevVersion    = "dev"
	UnknownCommit = "unknown"
	UnknownDate   = "unknown"
)

// Info is the resolved build identity.
type Info struct {
	Version string
	Commit  string
	Date    string
}

// Read resolves the build identity from the stamped values and the running
// binary's build info.
func Read(version, commit, date string) Info {
	bi, _ := debug.ReadBuildInfo()
	return Resolve(version, commit, date, bi)
}

// Resolve keeps every stamped value (a full commit hash abbreviated) and
// fills the ones left at their defaults from bi: the main module's version as
// a release version, the short VCS revision with -dirty for a modified working
// tree, the commit time.
func Resolve(version, commit, date string, bi *debug.BuildInfo) Info {
	out := Info{Version: version, Commit: commit, Date: date}
	if !unset(out.Commit, UnknownCommit) {
		out.Commit = shortRevision(out.Commit)
	}
	if bi == nil {
		return out
	}
	if unset(out.Version, DevVersion) {
		if v := moduleVersion(bi.Main.Version); v != "" {
			out.Version = v
		}
	}
	settings := map[string]string{}
	for _, s := range bi.Settings {
		settings[s.Key] = s.Value
	}
	if rev := settings["vcs.revision"]; unset(out.Commit, UnknownCommit) && rev != "" {
		out.Commit = shortRevision(rev)
		if settings["vcs.modified"] == "true" {
			out.Commit += "-dirty"
		}
	}
	if t := settings["vcs.time"]; unset(out.Date, UnknownDate) && t != "" {
		out.Date = t
	}
	return out
}

func unset(v, def string) bool { return v == "" || v == def }

// moduleVersion is the main module's version as a release version: without
// its v, never the toolchain's "(devel)" placeholder, and without the +dirty
// the toolchain appends for a modified working tree — a CI checkout carries
// build artefacts when the binary is built, so the suffix says nothing about
// the source; the commit keeps the dirty marker for a local build.
func moduleVersion(v string) string {
	v = strings.TrimSuffix(strings.TrimPrefix(v, "v"), "+dirty")
	if v == "(devel)" {
		return ""
	}
	return v
}

// shortRevision abbreviates a commit hash the way `git rev-parse --short`
// does.
func shortRevision(rev string) string {
	if len(rev) > 7 {
		return rev[:7]
	}
	return rev
}
