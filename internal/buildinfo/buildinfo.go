// Package buildinfo is which build a command is, for its -version flag:
// read from what Go stamps into the binary, never from a constant kept
// by hand. A build of a tagged commit says the tag (v0.1.0); any other
// build says the commit it was built from, with +dirty when the tree
// had uncommitted changes.
package buildinfo

import (
	"regexp"
	"runtime/debug"
)

// Version is this build's version.
func Version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	return version(bi)
}

var tag = regexp.MustCompile(`^v\d+\.\d+\.\d+(\+dirty)?$`)

// version is Version from bi: the main module's version when it is a
// release tag (Go stamps it from the VCS tag at the commit, "+dirty"
// when the tree was modified); otherwise the VCS revision, since a
// pseudo-version or "(devel)" says less than the commit does.
func version(bi *debug.BuildInfo) string {
	if tag.MatchString(bi.Main.Version) {
		return bi.Main.Version
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		if bi.Main.Version != "" {
			return bi.Main.Version
		}
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		rev += "+dirty"
	}
	return rev
}
