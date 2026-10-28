//go:build windows

// Package console is for a program built as a windowed one (-H=windowsgui),
// so that it starts at sign-in without a console window: run from a
// prompt, it writes to that prompt's console instead.
package console

import (
	"os"

	"golang.org/x/sys/windows"
)

var attachConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")

// UseParent attaches the program to the console it was started from, if
// there is one, and points whichever of its standard streams nothing
// else set at that console. A stream the parent redirected (a script's
// log file) stays where it was. It reports whether standard error goes
// anywhere afterwards: not from a scheduled task, say, when the program
// should log to a file of its own.
func UseParent() bool {
	const attachParent = ^uintptr(0) // ATTACH_PARENT_PROCESS
	attachConsole.Call(attachParent)
	if !valid(windows.STD_OUTPUT_HANDLE) {
		if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stdout = f
		}
	}
	if !valid(windows.STD_ERROR_HANDLE) {
		if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stderr = f
		}
	}
	if !valid(windows.STD_INPUT_HANDLE) {
		if f, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
			os.Stdin = f
		}
	}
	return HaveStderr()
}

// HaveStderr reports whether standard error goes anywhere: a console,
// or a file or pipe the parent gave. A windowed program started by the
// task scheduler has none.
func HaveStderr() bool {
	if valid(windows.STD_ERROR_HANDLE) {
		return true
	}
	fd := os.Stderr.Fd() // UseParent's console, or nothing (handle 0)
	return fd != 0 && fd != uintptr(windows.InvalidHandle)
}

// valid reports whether the process's standard handle of that kind is one.
func valid(kind uint32) bool {
	h, err := windows.GetStdHandle(kind)
	return err == nil && h != 0 && h != windows.InvalidHandle
}
