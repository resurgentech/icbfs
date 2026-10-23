//go:build windows

package objstore

import "golang.org/x/sys/windows"

// processAlive reports whether a process with this pid is still running.
func processAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}

// notOthersHere is a no-op on Windows: a process only ever finds its
// own user's cache directory (%LOCALAPPDATA%), even as SYSTEM.
func notOthersHere(string) error { return nil }
