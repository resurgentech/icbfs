//go:build windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

// install starts the tray when this user logs in: it runs in the user's
// session, where the user's mapped drives are.
func install() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue("icbfs-sync", fmt.Sprintf(`"%s" tray`, exe))
}

func uninstall() error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if err := k.DeleteValue("icbfs-sync"); err != nil && err != registry.ErrNotExist {
		return err
	}
	return nil
}
