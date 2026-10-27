//go:build !windows

package clicmd

import "errors"

// defaultRootSDDL has no account to take a default from off Windows.
func defaultRootSDDL() (string, error) {
	return "", errors.New("a primary-Windows filesystem needs -root-owner <SID> or -root-sddl when created off Windows")
}
