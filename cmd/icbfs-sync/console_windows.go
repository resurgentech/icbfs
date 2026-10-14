//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

var attachConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")

// useParentConsole: icbfs-sync is built as a windowed program, so the
// tray starts at login without a console window; the other commands,
// run from a command prompt, write to that prompt's console instead.
func useParentConsole() {
	const attachParent = ^uintptr(0) // ATTACH_PARENT_PROCESS
	if r, _, _ := attachConsole.Call(attachParent); r == 0 {
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout, os.Stderr = f, f
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
		os.Stdin = f
	}
}
