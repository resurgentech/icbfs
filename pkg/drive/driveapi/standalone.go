package driveapi

import (
	"context"
	"crypto/subtle"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/gofiber/fiber/v3"

	"github.com/resurgentech/icbfs/pkg/drive"
	"github.com/resurgentech/icbfs/pkg/drive/icbfsdrive"
	pkgicbfs "github.com/resurgentech/icbfs/pkg/icbfs"
)

// Standalone is what host would supply, from a configuration file,
// for testing without host (cmd/drive-api): one static token, a fixed
// drive list, a profile per actor, and which permissions each actor has.
// The actor is named by the X-Drive-Actor header.
type Standalone struct {
	Listen string `json:"listen"`
	Token  string `json:"token"`
	// Buckets by registered id.
	Buckets map[string]pkgicbfs.S3 `json:"buckets"`
	// Drives by registry id.
	Drives map[string]StandaloneDrive `json:"drives"`
	// SideStore is the deployment's side store: where drafts go for a
	// drive whose bucket has none of its own (and for drives with no
	// bucket at all).
	SideStore *SideStoreAt `json:"sideStore,omitempty"`
	// BucketSideStores gives a bucket its own side store, a filesystem in
	// that bucket (bucket id → filesystem name), so its drives' drafts
	// stay in the same storage as their documents.
	BucketSideStores map[string]string `json:"bucketSideStores,omitempty"`
	// Profiles by actor, for every drive...
	Profiles map[string]drive.Profile `json:"profiles"`
	// ... unless a drive has its own (drive id → actor → profile).
	DriveProfiles map[string]map[string]drive.Profile `json:"driveProfiles,omitempty"`
	// Permissions by actor: patterns like "drive.*", "storage.read", "*".
	// An actor not listed has every permission.
	Permissions map[string][]string `json:"permissions,omitempty"`
	CacheDir    string              `json:"cacheDir,omitempty"`
}

// SideStoreAt is a side store: a filesystem in a bucket.
type SideStoreAt struct {
	Bucket     string `json:"bucket"`
	Filesystem string `json:"filesystem"`
}

// StandaloneDrive is one configured drive.
type StandaloneDrive struct {
	Name       string `json:"name"`
	Bucket     string `json:"bucket"`
	Filesystem string `json:"filesystem"`
	Root       string `json:"root"`
	Writable   bool   `json:"writable"`
}

// Config turns the standalone configuration into the handlers' Config.
// Drives are attached on first use and kept.
func (st *Standalone) Config(ctx context.Context) (Config, func(), error) {
	stores := map[string]pkgicbfs.Store{}
	for id, b := range st.Buckets {
		stores[id] = pkgicbfs.OpenS3(b)
	}
	var mu sync.Mutex
	open := map[string]*icbfsdrive.Drive{}
	cfg := Config{
		Bucket: func(_ context.Context, id string) (pkgicbfs.Store, error) {
			s, ok := stores[id]
			if !ok {
				return nil, fmt.Errorf("bucket %s: %w", id, drive.ErrNotFound)
			}
			return s, nil
		},
		Drive: func(ctx context.Context, id string) (drive.Drive, DriveEntry, error) {
			sd, ok := st.Drives[id]
			if !ok {
				return nil, DriveEntry{}, fmt.Errorf("drive %s: %w", id, drive.ErrNotFound)
			}
			entry := DriveEntry{Name: sd.Name, Kind: "icbfs", Writable: sd.Writable}
			mu.Lock()
			defer mu.Unlock()
			if d, ok := open[id]; ok {
				return d, entry, nil
			}
			store, ok := stores[sd.Bucket]
			if !ok {
				return nil, DriveEntry{}, fmt.Errorf("drive %s's bucket %s: %w", id, sd.Bucket, drive.ErrNotFound)
			}
			d, err := icbfsdrive.New(ctx, icbfsdrive.Config{Store: store, Filesystem: sd.Filesystem, Root: sd.Root, ID: id, CacheDir: st.CacheDir})
			if err != nil {
				return nil, DriveEntry{}, err
			}
			open[id] = d
			return d, entry, nil
		},
		Authorize: func(c fiber.Ctx, driveID, permission string) (string, drive.Profile, error) {
			actor, err := st.authenticate(c, permission)
			if err != nil {
				return "", drive.Profile{}, err
			}
			if _, ok := st.Drives[driveID]; !ok {
				return "", drive.Profile{}, fmt.Errorf("drive %s: %w", driveID, drive.ErrNotFound)
			}
			p, ok := st.DriveProfiles[driveID][actor]
			if !ok {
				p, ok = st.Profiles[actor]
			}
			if !ok {
				return "", drive.Profile{}, drive.ErrNoAccess
			}
			return actor, p, nil
		},
		AuthorizeStorage: func(c fiber.Ctx, bucket, permission string) (string, drive.Profile, error) {
			actor, err := st.authenticate(c, permission)
			if err != nil {
				return "", drive.Profile{}, err
			}
			return actor, st.Profiles[actor], nil
		},
	}
	// Side stores open (and are made, the first time) on first use.
	sides := map[SideStoreAt]*drive.Drafts{}
	var sideMu sync.Mutex
	openSide := func(ctx context.Context, at SideStoreAt) (*drive.Drafts, error) {
		sideMu.Lock()
		defer sideMu.Unlock()
		if ds, ok := sides[at]; ok {
			return ds, nil
		}
		store, ok := stores[at.Bucket]
		if !ok {
			return nil, fmt.Errorf("side store bucket %s isn't configured", at.Bucket)
		}
		side, err := icbfsdrive.OpenSideStore(ctx, store, at.Filesystem)
		if err != nil {
			return nil, fmt.Errorf("side store %s/%s: %w", at.Bucket, at.Filesystem, err)
		}
		ds := &drive.Drafts{Side: side}
		sides[at] = ds
		return ds, nil
	}
	for b := range st.BucketSideStores {
		if _, ok := stores[b]; !ok {
			return Config{}, nil, fmt.Errorf("bucketSideStores: bucket %s isn't configured", b)
		}
	}
	if st.SideStore != nil {
		if _, ok := stores[st.SideStore.Bucket]; !ok {
			return Config{}, nil, fmt.Errorf("side store bucket %s isn't configured", st.SideStore.Bucket)
		}
	}
	cfg.Drafts = func(ctx context.Context, driveID string) (*drive.Drafts, error) {
		sd, ok := st.Drives[driveID]
		if !ok {
			return nil, fmt.Errorf("drive %s: %w", driveID, drive.ErrNotFound)
		}
		if fs, ok := st.BucketSideStores[sd.Bucket]; ok {
			return openSide(ctx, SideStoreAt{Bucket: sd.Bucket, Filesystem: fs})
		}
		if st.SideStore != nil {
			return openSide(ctx, *st.SideStore)
		}
		return nil, nil
	}
	cleanup := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, d := range open {
			d.Close()
		}
	}
	return cfg, cleanup, nil
}

// errUnauthenticated: no or a wrong token.
var errUnauthenticated = fmt.Errorf("unauthenticated: %w", drive.ErrForbidden)

func (st *Standalone) authenticate(c fiber.Ctx, permission string) (string, error) {
	auth := c.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(st.Token)) != 1 {
		return "", errUnauthenticated
	}
	actor := c.Get("X-Drive-Actor")
	if actor == "" {
		return "", fmt.Errorf("X-Drive-Actor is required: %w", drive.ErrInvalid)
	}
	perms, listed := st.Permissions[actor]
	if !listed {
		return actor, nil
	}
	for _, pat := range perms {
		if ok, _ := path.Match(pat, permission); ok || pat == "*" {
			return actor, nil
		}
	}
	return "", fmt.Errorf("%s may not %s: %w", actor, permission, drive.ErrForbidden)
}
