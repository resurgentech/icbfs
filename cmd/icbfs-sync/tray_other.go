//go:build !windows

package main

import "errors"

func tray(Paths) error { return errors.New("the tray is for Windows; use icbfs-sync run") }
