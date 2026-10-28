//go:build !windows

package console

// UseParent does nothing: every program has a console here.
func UseParent() bool { return true }

// HaveStderr is true: standard error always goes somewhere here.
func HaveStderr() bool { return true }
