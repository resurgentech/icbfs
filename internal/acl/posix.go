package acl

// POSIX permission bits, as in a mode's owner, group and other triplets.
const (
	PosixRead  uint32 = 4
	PosixWrite uint32 = 2
	PosixExec  uint32 = 1
)

// POSIXUser is who is asking on a primary-POSIX filesystem.
type POSIXUser struct {
	Uid  uint32
	Gids []uint32
}

// POSIXAllows reports whether u may do want (PosixRead|PosixWrite|PosixExec)
// to an object with mode, uid and gid, as the kernel decides: the owner's
// bits for the owner, else the group's for a member, else other's. uid 0
// may do anything, except execute a file with no execute bit at all.
func POSIXAllows(mode, uid, gid uint32, isDir bool, u POSIXUser, want uint32) bool {
	if u.Uid == 0 {
		if want&PosixExec != 0 && !isDir && mode&0o111 == 0 {
			return false
		}
		return true
	}
	var bits uint32
	switch {
	case u.Uid == uid:
		bits = mode >> 6
	case u.member(gid):
		bits = mode >> 3
	default:
		bits = mode
	}
	return bits&7&want == want
}

func (u POSIXUser) member(gid uint32) bool {
	for _, g := range u.Gids {
		if g == gid {
			return true
		}
	}
	return false
}
