package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config is icbfs-sync's settings file.
type Config struct {
	Store Store  `json:"store"`
	Pairs []Pair `json:"pairs"`
}

// Store is how to reach the bucket the drives are in.
type Store struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"accessKey"`
	// SecretKey in the clear, or SecretKeyProtected, encrypted for this
	// Windows account (icbfs-sync set-secret writes it).
	SecretKey          string `json:"secretKey,omitempty"`
	SecretKeyProtected string `json:"secretKeyProtected,omitempty"`
}

// Pair is a folder kept in step with an icbfs drive.
type Pair struct {
	Name string `json:"name"`
	// Folder is the directory on this machine: a local path or a mapped
	// drive (Z:\).
	Folder string `json:"folder"`
	// Filesystem and Root are the drive: an icbfs filesystem, and the
	// directory in it ("/" for all of it).
	Filesystem string `json:"filesystem"`
	Root       string `json:"root,omitempty"`
	// Mode is "one-way" (the folder is the truth) or "two-way".
	Mode string `json:"mode"`
	// Interval between passes ("5m"); RemoteEvery, for one-way, how often
	// the drive is listed too ("1h").
	Interval    string `json:"interval,omitempty"`
	RemoteEvery string `json:"remoteEvery,omitempty"`
	// MaxRemovals: a pass removing more files waits to be confirmed.
	MaxRemovals int `json:"maxRemovals,omitempty"`
	// RequirePresent: paths that must be in the folder for a pass to go
	// ahead (a file that shows a network drive is really there).
	RequirePresent []string `json:"requirePresent,omitempty"`
	// Ignore: name patterns added to the defaults ("*.url", "Scratch"),
	// matched against each part of a path, case-insensitively; a matching
	// directory is skipped with everything in it. A RequirePresent marker
	// can be ignored too: it's checked before names are ignored.
	Ignore []string `json:"ignore,omitempty"`
	// IDs and ACLs: "auto" (ask the volume), "on", "off".
	IDs  string `json:"ids,omitempty"`
	ACLs string `json:"acls,omitempty"`
	// TimeResolution, if set ("1s"), is how finely the folder keeps
	// times, instead of what the volume says.
	TimeResolution string `json:"timeResolution,omitempty"`
	// CaseSensitive pairs tell names apart by case; the default doesn't,
	// as Windows doesn't.
	CaseSensitive bool `json:"caseSensitive,omitempty"`
	Paused        bool `json:"paused,omitempty"`
}

func (p Pair) interval() time.Duration { return durationOr(p.Interval, 5*time.Minute) }

func (p Pair) remoteEvery() time.Duration { return durationOr(p.RemoteEvery, time.Hour) }

func durationOr(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}

func (c *Config) validate() error {
	if c.Store.Endpoint == "" || c.Store.Bucket == "" || c.Store.AccessKey == "" {
		return errors.New("store: endpoint, bucket and accessKey are required")
	}
	seen := map[string]bool{}
	for i, p := range c.Pairs {
		switch {
		case p.Name == "":
			return fmt.Errorf("pair %d: a name is required", i+1)
		case seen[p.Name]:
			return fmt.Errorf("pair %q: the name is used twice", p.Name)
		case p.Folder == "" || p.Filesystem == "":
			return fmt.Errorf("pair %q: folder and filesystem are required", p.Name)
		case p.Mode != "one-way" && p.Mode != "two-way":
			return fmt.Errorf("pair %q: mode is one-way or two-way", p.Name)
		}
		seen[p.Name] = true
	}
	return nil
}

// Paths are where icbfs-sync keeps things.
type Paths struct {
	Config string // the settings file
	Data   string // state databases, the spool, the log
}

func defaultPaths() Paths {
	cfg, err := os.UserConfigDir()
	if err != nil {
		cfg = "."
	}
	data, err := os.UserCacheDir() // %LOCALAPPDATA% on Windows
	if err != nil {
		data = cfg
	}
	return Paths{Config: filepath.Join(cfg, "icbfs-sync", "config.json"), Data: filepath.Join(data, "icbfs-sync")}
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if c.Store.SecretKeyProtected != "" {
		s, err := unprotect(c.Store.SecretKeyProtected)
		if err != nil {
			return nil, fmt.Errorf("%s: the protected secret key: %w", path, err)
		}
		c.Store.SecretKey = s
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// saveConfig writes c, keeping a protected secret protected.
func saveConfig(path string, c *Config) error {
	out := *c
	if out.Store.SecretKeyProtected != "" {
		out.Store.SecretKey = ""
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// template is what init writes: a starting point to edit.
func template() *Config {
	return &Config{
		Store: Store{Endpoint: "https://s3.example.com", Region: "us-east-1", Bucket: "documents", AccessKey: "ACCESS-KEY", SecretKey: "SECRET-KEY"},
		Pairs: []Pair{{
			Name: "documents", Folder: `Z:\`, Filesystem: "documents", Root: "/", Mode: "one-way",
			Interval: "5m", RemoteEvery: "1h", MaxRemovals: 200, IDs: "auto", ACLs: "auto",
			Ignore: []string{"*.url"},
		}},
	}
}

// loadConfigRaw parses without decrypting or checking, for editing.
func loadConfigRaw(data []byte) (*Config, error) {
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
