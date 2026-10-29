package drive

import (
	"context"
	"time"
)

// Profile is who an actor is on a drive's storage: on a primary-Windows
// filesystem a SID and its groups' SIDs, on a primary-POSIX one a uid and
// its gids. The host application maps actors to profiles; a drive refuses an actor
// without one (ErrNoAccess). System skips the file-level check entirely,
// for the library's own housekeeping and for tests — never for a user.
type Profile struct {
	Windows *WindowsProfile `json:"windows,omitempty"`
	POSIX   *POSIXProfile   `json:"posix,omitempty"`
	System  bool            `json:"system,omitempty"`
	// ByMode, for a profile with no POSIX identity on a primary-POSIX
	// filesystem, decides as a Windows mount of one does, having no uid to
	// match: an item can be read if any of its read bits is set, and
	// written if its owner-write bit is. For a Windows user of such a
	// filesystem, reading it through the library.
	ByMode bool `json:"byMode,omitempty"`
}

// WindowsProfile is a Windows identity.
type WindowsProfile struct {
	User   string   `json:"user"`             // the user's SID
	Groups []string `json:"groups,omitempty"` // its groups' SIDs
	Domain string   `json:"domain,omitempty"` // the domain SID domain-relative aliases (DA, DU, ...) resolve against
}

// POSIXProfile is a POSIX identity; Gids[0] is the primary group.
type POSIXProfile struct {
	Uid  uint32   `json:"uid"`
	Gids []uint32 `json:"gids,omitempty"`
}

type (
	actorKey      struct{}
	profileKey    struct{}
	lockHolderKey struct{}
	passKey       struct{}
	sourceTimeKey struct{}
)

// WithSourceTime says that the changes made under ctx happened at t at
// their source, not now: a mirror copying a source's history. Write,
// Mkdir, Move, Delete, SetTimes, SetOrigin and Restore honour it: what
// they write is dated t, so Versions, Trash and views of the past (At)
// show the source's history. Changes come in any order: one dated behind
// its item's current state goes into the past at t, and the present
// stays as it was (a file's current version is put back on top; a
// folder's copies from t on gain the change). t can't be in the future.
func WithSourceTime(ctx context.Context, t time.Time) context.Context {
	return context.WithValue(ctx, sourceTimeKey{}, t)
}

// SourceTime is ctx's source time, if it has one.
func SourceTime(ctx context.Context) (time.Time, bool) {
	t, ok := ctx.Value(sourceTimeKey{}).(time.Time)
	return t, ok
}

// WithPass attaches a sync pass's id to ctx: what a Delete with it sends to
// the trash is recorded as that pass's, so the pass's removals can be
// listed (TrashOptions.Pass) and undone together (Mirror.Undo).
func WithPass(ctx context.Context, pass string) context.Context {
	return context.WithValue(ctx, passKey{}, pass)
}

// Pass is ctx's sync pass, or "".
func Pass(ctx context.Context) string {
	p, _ := ctx.Value(passKey{}).(string)
	return p
}

// WithActor attaches who is acting (the host's user or agent id), for
// the records operations leave: trash entries, names, drafts, origins.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// Actor is ctx's actor, or "".
func Actor(ctx context.Context) string {
	a, _ := ctx.Value(actorKey{}).(string)
	return a
}

// WithLockHolder names the lock holder (Locker) whose locks writes
// under ctx may pass: a Write, Move or Delete of an item locked by
// anyone else is ErrLocked, and so is one under a context with no
// holder.
func WithLockHolder(ctx context.Context, holder string) context.Context {
	return context.WithValue(ctx, lockHolderKey{}, holder)
}

// LockHolder is ctx's lock holder, or "".
func LockHolder(ctx context.Context) string {
	h, _ := ctx.Value(lockHolderKey{}).(string)
	return h
}

// WithProfile attaches the profile the actor acts as on the drive.
func WithProfile(ctx context.Context, p Profile) context.Context {
	return context.WithValue(ctx, profileKey{}, p)
}

// ProfileFrom is ctx's profile, if it has one.
func ProfileFrom(ctx context.Context) (Profile, bool) {
	p, ok := ctx.Value(profileKey{}).(Profile)
	return p, ok
}

// AsSystem is the context a program that owns a drive acts under: the
// system profile, which no item's permissions refuse, with actor as who
// acts (WithActor). For a daemon mirroring into its own filesystem, a
// sync engine, an administrative tool; never for a user's request, which
// gets the user's own Profile.
func AsSystem(ctx context.Context, actor string) context.Context {
	return WithProfile(WithActor(ctx, actor), Profile{System: true})
}
