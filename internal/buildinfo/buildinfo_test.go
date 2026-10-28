package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestVersion(t *testing.T) {
	bi := func(main string, settings ...string) *debug.BuildInfo {
		b := &debug.BuildInfo{Main: debug.Module{Version: main}}
		for i := 0; i+1 < len(settings); i += 2 {
			b.Settings = append(b.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
		}
		return b
	}
	rev := "2dc211c8661be45411a662bcbb7b787413374b98"
	for _, c := range []struct {
		name string
		bi   *debug.BuildInfo
		want string
	}{
		{"a tag", bi("v0.1.0", "vcs.revision", rev, "vcs.modified", "false"), "v0.1.0"},
		{"a tag, dirty", bi("v0.1.0+dirty", "vcs.revision", rev, "vcs.modified", "true"), "v0.1.0+dirty"},
		{"past the tag: the commit, not the pseudo-version", bi("v0.1.1-0.20261028120000-2dc211c8661b", "vcs.revision", rev, "vcs.modified", "false"), "2dc211c8661b"},
		{"no tag", bi("(devel)", "vcs.revision", rev, "vcs.modified", "false"), "2dc211c8661b"},
		{"no tag, dirty", bi("(devel)", "vcs.revision", rev, "vcs.modified", "true"), "2dc211c8661b+dirty"},
		{"a dependency's build: no VCS, a version", bi("v0.2.0-beta"), "v0.2.0-beta"},
		{"nothing", bi(""), "unknown"},
	} {
		if got := version(c.bi); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
