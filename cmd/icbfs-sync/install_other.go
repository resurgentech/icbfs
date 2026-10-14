//go:build !windows

package main

import "errors"

func install() error {
	return errors.New("install starts the tray at login, on Windows; elsewhere run icbfs-sync run from your session's autostart")
}

func uninstall() error { return install() }
