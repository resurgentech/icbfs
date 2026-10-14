// Package acl evaluates the permissions icbfs stores, for access layers
// with no operating system to do it for them (the drive API): Windows
// security descriptors (SDDL) on primary-Windows filesystems, POSIX modes
// on primary-POSIX ones.
//
// The Windows check follows Windows' own rules for a file's DACL: each
// requested right is decided by the first entry, in order, that names it
// for one of the caller's SIDs — so a deny entry before an allow wins,
// as in a canonically ordered DACL, and an allow before a deny wins in a
// non-canonical one, as Windows does. The owner always has READ_CONTROL
// and WRITE_DAC unless an OWNER RIGHTS entry says otherwise. No DACL at
// all grants everything; an empty one grants nothing. Generic rights are
// mapped to file rights. Audit (SACL), object and conditional entries are
// kept but not evaluated, and privileges (backup, restore, take
// ownership) aren't modeled.
package acl

import (
	"fmt"
	"strconv"
	"strings"
)

// Rights bits (Windows access masks for files and directories).
const (
	ReadData        uint32 = 0x0001 // list a directory
	WriteData       uint32 = 0x0002 // add a file to a directory
	AppendData      uint32 = 0x0004 // add a subdirectory
	ReadEA          uint32 = 0x0008
	WriteEA         uint32 = 0x0010
	Execute         uint32 = 0x0020 // traverse a directory
	DeleteChild     uint32 = 0x0040
	ReadAttributes  uint32 = 0x0080
	WriteAttributes uint32 = 0x0100
	Delete          uint32 = 0x00010000
	ReadControl     uint32 = 0x00020000
	WriteDAC        uint32 = 0x00040000
	WriteOwner      uint32 = 0x00080000
	Synchronize     uint32 = 0x00100000

	GenericAll     uint32 = 0x10000000
	GenericExecute uint32 = 0x20000000
	GenericWrite   uint32 = 0x40000000
	GenericRead    uint32 = 0x80000000

	FileGenericRead    uint32 = 0x120089
	FileGenericWrite   uint32 = 0x120116
	FileGenericExecute uint32 = 0x1200A0
	FileAllAccess      uint32 = 0x1F01FF
)

// MapGeneric replaces generic rights with the file rights they stand for.
func MapGeneric(mask uint32) uint32 {
	if mask&GenericRead != 0 {
		mask |= FileGenericRead
	}
	if mask&GenericWrite != 0 {
		mask |= FileGenericWrite
	}
	if mask&GenericExecute != 0 {
		mask |= FileGenericExecute
	}
	if mask&GenericAll != 0 {
		mask |= FileAllAccess
	}
	return mask &^ (GenericRead | GenericWrite | GenericExecute | GenericAll)
}

// ACE types and flags.
const (
	Allow = "A"
	Deny  = "D"
)

const (
	ObjectInherit    uint8 = 1 << iota // OI: files below inherit it
	ContainerInherit                   // CI: directories below inherit it
	NoPropagate                        // NP: only one level down
	InheritOnly                        // IO: a template for children, not for this object
	Inherited                          // ID: this entry came from a parent
	auditSuccess                       // SA
	auditFailure                       // FA
)

var aceFlagNames = []struct {
	name string
	bit  uint8
}{{"OI", ObjectInherit}, {"CI", ContainerInherit}, {"NP", NoPropagate}, {"IO", InheritOnly}, {"ID", Inherited}, {"SA", auditSuccess}, {"FA", auditFailure}}

// ACE is one entry of a DACL.
type ACE struct {
	Type  string // Allow, Deny, or another SDDL type kept but not evaluated
	Flags uint8
	Mask  uint32
	SID   string // a SID string, or an SDDL alias (e.g. "DU") that needs a domain to resolve
	// Object and conditional ACE fields, kept verbatim for round-tripping.
	ObjectGUID, InheritGUID, Extra string
}

// SecurityDescriptor is a parsed SDDL security descriptor.
type SecurityDescriptor struct {
	Owner, Group string
	HasDACL      bool // false: no DACL at all, which grants everyone everything
	DACLFlags    string
	DACL         []ACE
	SACL         string // kept verbatim
}

// Protected reports whether the DACL blocks inheritance from its parent.
func (sd *SecurityDescriptor) Protected() bool { return strings.Contains(sd.DACLFlags, "P") }

// Well-known SIDs and SDDL aliases.
var aliasSIDs = map[string]string{
	"WD": "S-1-1-0", "CO": "S-1-3-0", "CG": "S-1-3-1", "OW": "S-1-3-4",
	"NU": "S-1-5-2", "IU": "S-1-5-4", "SU": "S-1-5-6", "AN": "S-1-5-7",
	"PS": "S-1-5-10", "AU": "S-1-5-11", "RC": "S-1-5-12", "SY": "S-1-5-18",
	"LS": "S-1-5-19", "NS": "S-1-5-20", "WR": "S-1-5-33",
	"BA": "S-1-5-32-544", "BU": "S-1-5-32-545", "BG": "S-1-5-32-546",
	"PU": "S-1-5-32-547", "AO": "S-1-5-32-548", "SO": "S-1-5-32-549",
	"PO": "S-1-5-32-550", "BO": "S-1-5-32-551", "RE": "S-1-5-32-552",
	"RU": "S-1-5-32-554", "RD": "S-1-5-32-555", "NO": "S-1-5-32-556",
	"IS": "S-1-5-32-568", "ER": "S-1-5-32-573", "AC": "S-1-15-2-1",
}

// Domain-relative aliases: the domain's SID plus this RID.
var domainRIDs = map[string]string{
	"LA": "500", "LG": "501", "DA": "512", "DU": "513", "DG": "514",
	"DC": "515", "DD": "516", "CA": "517", "SA": "518", "EA": "519",
	"PA": "520", "RO": "521",
}

// Well-known SIDs the check treats specially.
const (
	everyone      = "S-1-1-0"
	creatorOwner  = "S-1-3-0"
	creatorGroup  = "S-1-3-1"
	ownerRights   = "S-1-3-4"
	authenticated = "S-1-5-11"
)

var rightNames = map[string]uint32{
	"GA": GenericAll, "GR": GenericRead, "GW": GenericWrite, "GX": GenericExecute,
	"RC": ReadControl, "SD": Delete, "WD": WriteDAC, "WO": WriteOwner,
	"FA": FileAllAccess, "FR": FileGenericRead, "FW": FileGenericWrite, "FX": FileGenericExecute,
	"CC": 0x1, "DC": 0x2, "LC": 0x4, "SW": 0x8, "RP": 0x10, "WP": 0x20, "DT": 0x40, "LO": 0x80, "CR": 0x100,
}

// Parse parses an SDDL string: O:owner G:group D:flags(aces) S:flags(aces),
// each part optional, in that order.
func Parse(sddl string) (*SecurityDescriptor, error) {
	sd := &SecurityDescriptor{}
	rest := strings.TrimSpace(sddl)
	bad := func(why string) (*SecurityDescriptor, error) {
		return nil, fmt.Errorf("sddl %q: %s", sddl, why)
	}
	for rest != "" {
		if len(rest) < 2 || rest[1] != ':' {
			return bad("expected O:, G:, D: or S: at " + strconv.Quote(rest))
		}
		part := rest[0]
		rest = rest[2:]
		switch part {
		case 'O', 'G':
			sid, n := sidToken(rest)
			if n == 0 {
				return bad("expected a SID after " + string(part) + ":")
			}
			if part == 'O' {
				sd.Owner = sid
			} else {
				sd.Group = sid
			}
			rest = rest[n:]
		case 'D', 'S':
			end := aclEnd(rest)
			value := rest[:end]
			rest = rest[end:]
			if part == 'S' {
				sd.SACL = value
				continue
			}
			flags, aces, err := parseACL(value)
			if err != nil {
				return bad(err.Error())
			}
			sd.HasDACL, sd.DACLFlags, sd.DACL = true, flags, aces
		default:
			return bad("unknown part " + string(part) + ":")
		}
	}
	return sd, nil
}

// sidToken reads a SID at the start of s: "S-1-…" (digits and dashes) or
// a two-letter alias. It returns the SID and how many bytes it took.
func sidToken(s string) (string, int) {
	if strings.HasPrefix(s, "S-") {
		n := 2
		for n < len(s) && (s[n] == '-' || s[n] >= '0' && s[n] <= '9') {
			n++
		}
		return s[:n], n
	}
	if len(s) >= 2 && isUpper(s[0]) && isUpper(s[1]) {
		return s[:2], 2
	}
	return "", 0
}

func isUpper(c byte) bool { return c >= 'A' && c <= 'Z' }

// aclEnd is where an ACL's text ends: at "S:" outside parentheses, or the
// end of the string.
func aclEnd(s string) int {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		case 'S':
			if depth == 0 && i+1 < len(s) && s[i+1] == ':' {
				return i
			}
		}
	}
	return len(s)
}

func parseACL(s string) (string, []ACE, error) {
	i := strings.IndexByte(s, '(')
	if i < 0 {
		return s, nil, nil // flags only: an empty DACL ("D:P")
	}
	flags := s[:i]
	var aces []ACE
	for s = s[i:]; s != ""; {
		if s[0] != '(' {
			return "", nil, fmt.Errorf("ace list at %q", s)
		}
		depth, j := 0, 0
		for ; j < len(s); j++ {
			if s[j] == '(' {
				depth++
			} else if s[j] == ')' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		if j == len(s) {
			return "", nil, fmt.Errorf("unterminated ace %q", s)
		}
		ace, err := parseACE(s[1:j])
		if err != nil {
			return "", nil, err
		}
		aces = append(aces, ace)
		s = s[j+1:]
	}
	return flags, aces, nil
}

func parseACE(s string) (ACE, error) {
	fields := strings.SplitN(s, ";", 7)
	if len(fields) < 6 {
		return ACE{}, fmt.Errorf("ace %q: want type;flags;rights;guid;guid;sid", s)
	}
	ace := ACE{Type: fields[0], ObjectGUID: fields[3], InheritGUID: fields[4], SID: fields[5]}
	if len(fields) == 7 {
		ace.Extra = fields[6]
	}
	for f := fields[1]; f != ""; f = f[2:] {
		if len(f) < 2 {
			return ACE{}, fmt.Errorf("ace %q: flags %q", s, fields[1])
		}
		found := false
		for _, n := range aceFlagNames {
			if f[:2] == n.name {
				ace.Flags |= n.bit
				found = true
			}
		}
		if !found {
			return ACE{}, fmt.Errorf("ace %q: unknown flag %q", s, f[:2])
		}
	}
	mask, err := parseRights(fields[2])
	if err != nil {
		return ACE{}, fmt.Errorf("ace %q: %w", s, err)
	}
	ace.Mask = mask
	return ace, nil
}

func parseRights(r string) (uint32, error) {
	if strings.HasPrefix(r, "0x") || strings.HasPrefix(r, "0X") {
		v, err := strconv.ParseUint(r[2:], 16, 32)
		return uint32(v), err
	}
	var mask uint32
	for ; r != ""; r = r[2:] {
		if len(r) < 2 {
			return 0, fmt.Errorf("rights %q", r)
		}
		bit, ok := rightNames[r[:2]]
		if !ok {
			return 0, fmt.Errorf("unknown right %q", r[:2])
		}
		mask |= bit
	}
	return mask, nil
}

// String renders sd as SDDL.
func (sd *SecurityDescriptor) String() string {
	var b strings.Builder
	if sd.Owner != "" {
		b.WriteString("O:" + sd.Owner)
	}
	if sd.Group != "" {
		b.WriteString("G:" + sd.Group)
	}
	if sd.HasDACL {
		b.WriteString("D:" + sd.DACLFlags)
		for _, a := range sd.DACL {
			b.WriteString("(" + a.String() + ")")
		}
	}
	if sd.SACL != "" {
		b.WriteString("S:" + sd.SACL)
	}
	return b.String()
}

func (a ACE) String() string {
	var flags strings.Builder
	for _, n := range aceFlagNames {
		if a.Flags&n.bit != 0 {
			flags.WriteString(n.name)
		}
	}
	s := fmt.Sprintf("%s;%s;0x%x;%s;%s;%s", a.Type, flags.String(), a.Mask, a.ObjectGUID, a.InheritGUID, a.SID)
	if a.Extra != "" {
		s += ";" + a.Extra
	}
	return s
}

// Principal is who is asking: a user SID and its groups' SIDs, and the
// domain SID that domain-relative aliases (DU, DA, ...) resolve against.
type Principal struct {
	User   string
	Groups []string
	Domain string // e.g. "S-1-5-21-1004336348-1177238915-682003330"; empty: domain aliases match nothing
}

// sids is every SID the principal counts as.
func (p Principal) sids() map[string]bool {
	m := map[string]bool{p.User: true, everyone: true, authenticated: true}
	for _, g := range p.Groups {
		m[g] = true
	}
	return m
}

// resolve turns an alias into a SID.
func (p Principal) resolve(sid string) string {
	if s, ok := aliasSIDs[sid]; ok {
		return s
	}
	if rid, ok := domainRIDs[sid]; ok && p.Domain != "" {
		return p.Domain + "-" + rid
	}
	return sid
}

// Rights is the principal's effective rights on an object with sd.
func (sd *SecurityDescriptor) Rights(p Principal) uint32 {
	if sd == nil || !sd.HasDACL {
		return FileAllAccess
	}
	sids := p.sids()
	isOwner := sd.Owner != "" && sids[p.resolve(sd.Owner)]
	var allowed, decided uint32
	ownerRightsSet := false
	for _, a := range sd.DACL {
		if a.Flags&InheritOnly != 0 || (a.Type != Allow && a.Type != Deny) {
			continue
		}
		sid := p.resolve(a.SID)
		if sid == ownerRights {
			ownerRightsSet = true
			if !isOwner {
				continue
			}
		} else if !sids[sid] {
			continue
		}
		bits := MapGeneric(a.Mask) &^ decided
		if a.Type == Allow {
			allowed |= bits
		}
		decided |= bits
	}
	if isOwner && !ownerRightsSet {
		allowed |= ReadControl | WriteDAC
	}
	return allowed
}

// Allows reports whether p has every right in want on an object with sd.
func (sd *SecurityDescriptor) Allows(p Principal, want uint32) bool {
	want = MapGeneric(want)
	return sd.Rights(p)&want == want
}

// Inherit is the security descriptor Windows gives a new object created
// in a directory with parent, when the creator supplies none: the
// parent's inheritable entries (OI for a file, CI for a directory, with
// NP stopping propagation), marked inherited, CREATOR OWNER / GROUP
// replaced by the new owner and group in the entries that apply to the
// object itself; owned by owner. With nothing to inherit, the owner
// gets full control.
func Inherit(parent *SecurityDescriptor, isDir bool, owner, group string) *SecurityDescriptor {
	sd := &SecurityDescriptor{Owner: owner, Group: group, HasDACL: true, DACLFlags: "AI"}
	if parent != nil && parent.HasDACL {
		for _, a := range parent.DACL {
			if a.Type != Allow && a.Type != Deny {
				continue
			}
			sd.DACL = append(sd.DACL, inheritACE(a, isDir, owner, group)...)
		}
	}
	if len(sd.DACL) == 0 {
		sd.DACL = []ACE{{Type: Allow, Mask: FileAllAccess, SID: owner}}
	}
	return sd
}

func inheritACE(a ACE, isDir bool, owner, group string) []ACE {
	inheritable := a.Flags&ObjectInherit != 0 && !isDir || a.Flags&ContainerInherit != 0 && isDir
	propagates := isDir && a.Flags&NoPropagate == 0 && a.Flags&(ObjectInherit|ContainerInherit) != 0
	if !inheritable && !(isDir && a.Flags&ObjectInherit != 0 && a.Flags&NoPropagate == 0) {
		return nil
	}
	creator := a.SID == "CO" || a.SID == creatorOwner || a.SID == "CG" || a.SID == creatorGroup
	var out []ACE
	if inheritable {
		eff := a
		eff.Flags = Inherited
		eff.Mask = MapGeneric(a.Mask)
		switch a.SID {
		case "CO", creatorOwner:
			eff.SID = owner
		case "CG", creatorGroup:
			eff.SID = group
		}
		if propagates && !creator {
			// one entry both applies here and carries on down
			eff.Flags |= a.Flags & (ObjectInherit | ContainerInherit)
			return append(out, eff)
		}
		out = append(out, eff)
	}
	if propagates {
		// a template for what's below, not applying here
		tmpl := a
		tmpl.Flags = a.Flags&(ObjectInherit|ContainerInherit) | InheritOnly | Inherited
		out = append(out, tmpl)
	}
	return out
}
