package drive

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// SideStore is where host keeps its own data about documents, on any
// drive: drafts now, review information later. Keys are '/'-separated;
// each object is versioned, with string metadata.
type SideStore interface {
	Put(ctx context.Context, key string, body []byte, meta map[string]string) error
	Get(ctx context.Context, key string) ([]byte, map[string]string, error)
	// Erase deletes an object with every version of it.
	Erase(ctx context.Context, key string) error
	// List lists the names directly under prefix (a '/'-ended key): keys
	// there, and the next part of longer ones.
	List(ctx context.Context, prefix string) ([]string, error)
}

// Drafts are autosaves kept apart from a file's versions: one per user
// and file, in the side store, under drafts/<drive>/<item>/<user>.
type Drafts struct {
	Side SideStore
}

// Draft describes a user's draft of a file.
type Draft struct {
	Drive  string    `json:"drive"`
	ItemID string    `json:"itemId"`
	User   string    `json:"user"`
	Base   string    `json:"baseVersion"` // the version the draft started from
	Saved  time.Time `json:"saved"`
	Size   int64     `json:"size"`
}

func draftDir(drive, item string) string {
	return "drafts/" + url.PathEscape(drive) + "/" + url.PathEscape(item) + "/"
}

func draftKey(drive, item, user string) string {
	return draftDir(drive, item) + url.PathEscape(user)
}

// Save stores user's draft of a file, started from version base.
func (ds *Drafts) Save(ctx context.Context, drive, item, user, base string, body []byte) (Draft, error) {
	if user == "" {
		return Draft{}, fmt.Errorf("a draft needs an actor: %w", ErrInvalid)
	}
	d := Draft{Drive: drive, ItemID: item, User: user, Base: base, Saved: time.Now().UTC(), Size: int64(len(body))}
	meta := map[string]string{"base": base, "saved": d.Saved.Format(time.RFC3339Nano), "user": user}
	if err := ds.Side.Put(ctx, draftKey(drive, item, user), body, meta); err != nil {
		return Draft{}, err
	}
	return d, nil
}

// Get is user's draft of a file.
func (ds *Drafts) Get(ctx context.Context, drive, item, user string) (Draft, []byte, error) {
	body, meta, err := ds.Side.Get(ctx, draftKey(drive, item, user))
	if err != nil {
		return Draft{}, nil, err
	}
	saved, _ := time.Parse(time.RFC3339Nano, meta["saved"])
	return Draft{Drive: drive, ItemID: item, User: user, Base: meta["base"], Saved: saved, Size: int64(len(body))}, body, nil
}

// Discard throws a draft away, with its old autosaves.
func (ds *Drafts) Discard(ctx context.Context, drive, item, user string) error {
	return ds.Side.Erase(ctx, draftKey(drive, item, user))
}

// Users is who has a draft of a file.
func (ds *Drafts) Users(ctx context.Context, drive, item string) ([]string, error) {
	keys, err := ds.Side.List(ctx, draftDir(drive, item))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if u, err := url.PathUnescape(k); err == nil {
			out = append(out, u)
		}
	}
	return out, nil
}

// ByItem is, for a drive, who has drafts of which files (item id →
// users), for the items in only (all when nil).
func (ds *Drafts) ByItem(ctx context.Context, drive string, only map[string]bool) (map[string][]string, error) {
	items, err := ds.Side.List(ctx, "drafts/"+url.PathEscape(drive)+"/")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return map[string][]string{}, nil
		}
		return nil, err
	}
	out := map[string][]string{}
	for _, esc := range items {
		item, err := url.PathUnescape(esc)
		if err != nil || only != nil && !only[item] {
			continue
		}
		users, err := ds.Users(ctx, drive, item)
		if err != nil {
			return nil, err
		}
		if len(users) > 0 {
			out[item] = users
		}
	}
	return out, nil
}

// DeleteAll erases every user's draft of a file (a purge does).
func (ds *Drafts) DeleteAll(ctx context.Context, drive, item string) error {
	users, err := ds.Users(ctx, drive, item)
	if err != nil {
		return err
	}
	for _, u := range users {
		if err := ds.Discard(ctx, drive, item, u); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}

// Publish makes user's draft the file's content — only if the file is
// still at the draft's base version, unless force — and discards it.
func (ds *Drafts) Publish(ctx context.Context, d Drive, drive, item, user string, force bool) (Item, error) {
	draft, body, err := ds.Get(ctx, drive, item, user)
	if err != nil {
		return Item{}, err
	}
	it, err := d.Stat(ctx, ItemRef{ID: item}, At{})
	if err != nil {
		return Item{}, err
	}
	cond := Condition{IfMatch: draft.Base}
	if force || draft.Base == "" {
		cond = Condition{}
	}
	out, err := d.Write(ctx, it.Path, bytes.NewReader(body), cond, WriteOptions{})
	if err != nil {
		return Item{}, err
	}
	if err := ds.Discard(ctx, drive, item, user); err != nil {
		return out, err
	}
	return out, nil
}
