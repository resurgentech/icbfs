package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// The mount(8) helper: installed as /sbin/mount.fuse.icbfs (a link to this
// binary), it makes "mount -t fuse.icbfs NAME DIR [-o OPTIONS]" mount the
// filesystem NAME on DIR, and so fstab entries and xfstests work. mount(8)
// runs it as
//
//	mount.fuse.icbfs NAME DIR [-n] [-v] [-o OPTIONS]
//
// It starts "icbfs mount" detached, waits for the mount to appear, and
// exits; umount ends it. The store connection comes from the ICBFS_*
// variables, or failing those from /etc/icbfs.env (KEY=VALUE lines).

const helperName = "mount.fuse.icbfs"

// helperConfig is read for any ICBFS_* variable not already set.
var helperConfig = "/etc/icbfs.env"

// mountOptionFlags maps mount -o options to icbfs mount flags.
var mountOptionFlags = map[string]string{
	"ro":          "-read-only",
	"allow_other": "-allow-other",
	"locking":     "-locking",
	"notify":      "-notify",
	"debug":       "-debug",
}

// valueOptions are mount -o options with a value, and their flags.
var valueOptions = map[string]string{
	"cache_dir":  "-cache-dir",
	"cache_size": "-cache-size",
	"as_of":      "-as-of",
}

// ignoredMountOptions are generic options mount(8) may pass that mean
// nothing here.
var ignoredMountOptions = map[string]bool{
	"rw": true, "defaults": true, "auto": true, "noauto": true, "user": true, "nouser": true,
	"dev": true, "nodev": true, "suid": true, "nosuid": true, "exec": true, "noexec": true,
	"atime": true, "noatime": true, "relatime": true, "strictatime": true, "nodiratime": true,
	"async": true, "_netdev": true, "nofail": true, "default_permissions": true,
}

func mountHelper(args []string) int {
	name, dir, flags, err := helperFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", helperName, err)
		return 1
	}
	loadHelperConfig(helperConfig)
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", helperName, err)
		return 1
	}
	logFile, err := os.CreateTemp("", "icbfs-mount-*.log")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", helperName, err)
		return 1
	}
	cmd := exec.Command(self, flags...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", helperName, err)
		return 1
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	abs, _ := filepath.Abs(dir)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			out, _ := os.ReadFile(logFile.Name())
			os.Remove(logFile.Name())
			fmt.Fprintf(os.Stderr, "%s: mounting %s failed:\n%s", helperName, name, out)
			return 1
		case <-time.After(100 * time.Millisecond):
		}
		if mountedAt(abs) {
			return 0 // the mount goes on logging to logFile
		}
	}
	_ = cmd.Process.Kill()
	fmt.Fprintf(os.Stderr, "%s: %s not mounted after 60s; log in %s\n", helperName, name, logFile.Name())
	return 1
}

// helperFlags turns mount(8)'s arguments into icbfs mount's.
func helperFlags(args []string) (name, dir string, flags []string, err error) {
	var pos, opts []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-o" && i+1 < len(args):
			opts = append(opts, strings.Split(args[i+1], ",")...)
			i++
		case strings.HasPrefix(a, "-o"):
			opts = append(opts, strings.Split(a[2:], ",")...)
		case a == "-n" || a == "-v" || a == "-s" || a == "-f":
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return "", "", nil, fmt.Errorf("usage: %s NAME DIR [-o OPTIONS]", helperName)
	}
	name, dir = pos[0], pos[1]
	flags = []string{"mount", "-fs", name}
	for _, o := range opts {
		key, value, hasValue := strings.Cut(o, "=")
		switch {
		case o == "":
		case mountOptionFlags[key] != "" && !hasValue:
			flags = append(flags, mountOptionFlags[key])
		case valueOptions[key] != "" && hasValue:
			flags = append(flags, valueOptions[key], value)
		case ignoredMountOptions[key]:
		default:
			return "", "", nil, fmt.Errorf("option %q not supported", o)
		}
	}
	return name, dir, append(flags, dir), nil
}

// mountedAt reports whether an icbfs mount is at dir.
func mountedAt(dir string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		// id parent major:minor root mountpoint options ... - fstype source superoptions
		f := strings.Fields(line)
		if len(f) < 5 || unescapeMountinfo(f[4]) != dir {
			continue
		}
		for i, x := range f {
			if x == "-" && i+1 < len(f) && f[i+1] == "fuse.icbfs" {
				return true
			}
		}
	}
	return false
}

// unescapeMountinfo undoes mountinfo's octal escapes (\040 for a space).
func unescapeMountinfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			var c byte
			if _, err := fmt.Sscanf(s[i+1:i+4], "%03o", &c); err == nil {
				b.WriteByte(c)
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// loadHelperConfig sets the ICBFS_* variables path defines that aren't
// already set. A missing file is fine.
func loadHelperConfig(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || !strings.HasPrefix(k, "ICBFS_") {
			continue
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, strings.Trim(v, `"'`))
		}
	}
}
