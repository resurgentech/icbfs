//go:build !windows

package main

import "errors"

func protect(string) (string, error) {
	return "", errors.New("protecting the secret key needs Windows; keep the settings file readable only by you instead")
}

func unprotect(string) (string, error) {
	return "", errors.New("a protected secret key can only be read on Windows, by the account that protected it")
}
