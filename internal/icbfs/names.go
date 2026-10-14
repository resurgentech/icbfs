package icbfs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"time"

	"github.com/resurgentech/icbfs/internal/objstore"
)

// NamedVersion is a name for a moment in a filesystem's history: mounting
// or reading "as of" the name is reading as of its time. Names are only
// created and deleted, never moved. Scope is the path of the subtree the
// name was made for ("" for the whole filesystem) — the drive API shows a
// name only to drives at or above it; the time itself covers everything.
type NamedVersion struct {
	Name      string    `json:"name"`
	Time      time.Time `json:"time"`
	Scope     string    `json:"scope,omitempty"`
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

var versionNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._ -]{0,62}$`)

func nameKey(fsID, name string) string { return fsID + "-name-" + name }

// CheckVersionName says whether name can name a version: letters,
// digits, '.', '_', '-' and spaces, up to 63, and not something -as-of
// would read as a time.
func CheckVersionName(name string) error {
	if !versionNameRE.MatchString(name) {
		return fmt.Errorf("version name %q: want letters, digits, '.', '_', '-' or spaces, starting with a letter or digit, up to 63", name)
	}
	if _, err := ParseAsOf(name, time.UTC); err == nil {
		return fmt.Errorf("version name %q reads as a time", name)
	}
	return nil
}

// CreateName names a moment of filesystem fsName: at, or — when at is
// zero — now, by the object store's clock (the name object's own
// version time, so it orders exactly against the writes before it).
// A time before the filesystem existed, or after now, is refused, and so
// is a name already taken (ErrExists).
func CreateName(ctx context.Context, store objstore.Store, fsName string, nv NamedVersion, at time.Time) (NamedVersion, error) {
	if err := CheckVersionName(nv.Name); err != nil {
		return NamedVersion{}, err
	}
	info, err := DescribeFilesystem(ctx, store, fsName)
	if err != nil {
		return NamedVersion{}, err
	}
	if !at.IsZero() {
		now, err := store.ServerTime(ctx)
		if err != nil {
			return NamedVersion{}, err
		}
		if at.After(now) {
			return NamedVersion{}, fmt.Errorf("name %q: %s is in the future", nv.Name, at.Format(time.RFC3339))
		}
		if !info.Created.IsZero() && at.Before(info.Created) {
			return NamedVersion{}, fmt.Errorf("name %q: filesystem %q didn't exist yet at %s", nv.Name, fsName, at.Format(time.RFC3339))
		}
	}
	nv.Time = at.UTC()
	nv.CreatedAt = time.Now().UTC()
	data, err := json.Marshal(nv)
	if err != nil {
		return NamedVersion{}, err
	}
	key := nameKey(info.ID, nv.Name)
	if _, err := store.PutIfAbsent(ctx, key, bytes.NewReader(data), nil); err != nil {
		if objstore.IsPreconditionFailed(err) {
			return NamedVersion{}, fmt.Errorf("version name %q: %w", nv.Name, ErrExists)
		}
		return NamedVersion{}, err
	}
	return readVersionName(ctx, store, key)
}

// readVersionName reads a name object; a name made for "now" takes its time
// from the object's version (the store's clock at the create).
func readVersionName(ctx context.Context, store objstore.Store, key string) (NamedVersion, error) {
	body, _, err := store.Get(ctx, key)
	if err != nil {
		return NamedVersion{}, mapNotFound(err)
	}
	data, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		return NamedVersion{}, err
	}
	var nv NamedVersion
	if err := json.Unmarshal(data, &nv); err != nil {
		return NamedVersion{}, err
	}
	if nv.Time.IsZero() {
		versions, err := store.ListVersions(ctx, key)
		if err != nil {
			return NamedVersion{}, err
		}
		if len(versions) == 0 || versions[0].DeleteMarker {
			return NamedVersion{}, fmt.Errorf("version name %q: %w", nv.Name, ErrNotFound)
		}
		nv.Time = versions[0].LastModified.UTC()
	}
	return nv, nil
}

// ResolveName looks up one name of filesystem fsName.
func ResolveName(ctx context.Context, store objstore.Store, fsName, name string) (NamedVersion, error) {
	info, err := DescribeFilesystem(ctx, store, fsName)
	if err != nil {
		return NamedVersion{}, err
	}
	nv, err := readVersionName(ctx, store, nameKey(info.ID, name))
	if err != nil {
		return NamedVersion{}, fmt.Errorf("version name %q: %w", name, err)
	}
	return nv, nil
}

// ListNames lists filesystem fsName's names, oldest moment first.
func ListNames(ctx context.Context, store objstore.Store, fsName string) ([]NamedVersion, error) {
	info, err := DescribeFilesystem(ctx, store, fsName)
	if err != nil {
		return nil, err
	}
	objs, err := store.ListByPrefix(ctx, nameKey(info.ID, ""))
	if err != nil {
		return nil, err
	}
	var out []NamedVersion
	for _, o := range objs {
		nv, err := readVersionName(ctx, store, o.Key)
		if err != nil {
			continue // deleted since the listing
		}
		out = append(out, nv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// DeleteName removes a name. What it named is untouched.
func DeleteName(ctx context.Context, store objstore.Store, fsName, name string) error {
	info, err := DescribeFilesystem(ctx, store, fsName)
	if err != nil {
		return err
	}
	key := nameKey(info.ID, name)
	if _, err := store.Head(ctx, key); err != nil {
		return fmt.Errorf("version name %q: %w", name, mapNotFound(err))
	}
	return store.Delete(ctx, key, "")
}

// ResolveAsOf turns an -as-of argument into a time: a time ParseAsOf
// reads, or else one of filesystem fsName's version names.
func ResolveAsOf(ctx context.Context, store objstore.Store, fsName, arg string, loc *time.Location) (time.Time, error) {
	if t, err := ParseAsOf(arg, loc); err == nil {
		return t, nil
	} else if CheckVersionName(arg) != nil {
		return time.Time{}, err
	}
	nv, err := ResolveName(ctx, store, fsName, arg)
	if err != nil {
		return time.Time{}, fmt.Errorf("as-of %q: not a time, and %w", arg, err)
	}
	return nv.Time, nil
}

