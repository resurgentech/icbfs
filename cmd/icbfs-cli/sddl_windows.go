//go:build windows

package main

import "github.com/winfsp/go-winfsp/procsd"

// defaultRootSDDL is the running account's own default security
// descriptor — what a Windows mount shows for a root without one.
func defaultRootSDDL() (string, error) {
	sd, err := procsd.Load()
	if err != nil {
		return "", err
	}
	return sd.String(), nil
}
