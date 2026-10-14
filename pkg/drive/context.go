package drive

import "context"

// Profile is who an actor is on a drive's storage: on a primary-Windows
// filesystem a SID and its groups' SIDs, on a primary-POSIX one a uid and
// its gids. Host maps actors to profiles; a drive refuses an actor
// without one (ErrNoAccess). System skips the file-level check entirely,
// for the library's own housekeeping and for tests — never for a user.
type Profile struct {
	Windows *WindowsProfile `json:"windows,omitempty"`
	POSIX   *POSIXProfile   `json:"posix,omitempty"`
	System  bool            `json:"system,omitempty"`
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
	actorKey   struct{}
	profileKey struct{}
)

// WithActor attaches who is acting (host's user or agent id), for
// the records operations leave: trash entries, names, drafts, origins.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// Actor is ctx's actor, or "".
func Actor(ctx context.Context) string {
	a, _ := ctx.Value(actorKey{}).(string)
	return a
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
