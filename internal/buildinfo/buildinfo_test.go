package buildinfo

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolve(t *testing.T) {
	tagged := &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/giantswarm/model-manager", Version: "v0.24.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "6e3caf2d9c1f4b7a8e2d0c5b6a7f8e9d0c1b2a3f"},
			{Key: "vcs.time", Value: "2026-09-16T08:00:00Z"},
			{Key: "vcs.modified", Value: "false"},
		},
	}
	untaggedDirty := &debug.BuildInfo{
		Main:     debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abcdef1234"}, {Key: "vcs.modified", Value: "true"}},
	}

	tests := map[string]struct {
		version, commit, date string
		bi                    *debug.BuildInfo
		want                  Info
	}{
		"tag at HEAD fills the ldflags defaults": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate, bi: tagged,
			want: Info{Version: "0.24.0", Commit: "6e3caf2", Date: "2026-09-16T08:00:00Z"},
		},
		"tag at HEAD in a modified tree: the version stays the release, the commit is marked": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate,
			bi: &debug.BuildInfo{
				Main:     debug.Module{Version: "v0.24.0+dirty"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "6e3caf2d9c1f4b7a8e2d0c5b6a7f8e9d0c1b2a3f"}, {Key: "vcs.modified", Value: "true"}},
			},
			want: Info{Version: "0.24.0", Commit: "6e3caf2-dirty", Date: "unknown"},
		},
		"ldflags values win": {
			version: "0.25.0", commit: "1234567", date: "2026-10-01T00:00:00Z", bi: tagged,
			want: Info{Version: "0.25.0", Commit: "1234567", Date: "2026-10-01T00:00:00Z"},
		},
		"untagged dirty checkout stays dev and marks the commit": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate, bi: untaggedDirty,
			want: Info{Version: "dev", Commit: "abcdef1-dirty", Date: "unknown"},
		},
		"no build info leaves everything as given": {
			version: DevVersion, commit: UnknownCommit, date: UnknownDate, bi: nil,
			want: Info{Version: "dev", Commit: "unknown", Date: "unknown"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, Resolve(tc.version, tc.commit, tc.date, tc.bi))
		})
	}
}
