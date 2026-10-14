package acl

import "testing"

const (
	alice   = "S-1-5-21-1-2-3-1001"
	bob     = "S-1-5-21-1-2-3-1002"
	finance = "S-1-5-21-1-2-3-2001"
	domain  = "S-1-5-21-1-2-3"
)

func mustParse(t *testing.T, s string) *SecurityDescriptor {
	t.Helper()
	sd, err := Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return sd
}

func TestParseAndRoundTrip(t *testing.T) {
	for _, s := range []string{
		"O:BAG:BAD:P(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;S-1-5-21-1-2-3-2001)",
		"O:S-1-5-21-1-2-3-1001G:DUD:PAI(A;OICIID;FA;;;SY)(D;;FW;;;WD)",
		"O:BAG:BAD:PS:AI(AU;SAFA;FA;;;WD)",
		"D:", // empty DACL
		"O:SY",
	} {
		sd := mustParse(t, s)
		again := mustParse(t, sd.String())
		if again.String() != sd.String() {
			t.Fatalf("round trip of %q: %q then %q", s, sd.String(), again.String())
		}
	}
	sd := mustParse(t, "O:BAG:DUD:PAI(A;OICI;FA;;;BA)(D;CIIO;WD;;;S-1-5-21-1-2-3-1001)")
	if sd.Owner != "BA" || sd.Group != "DU" || !sd.Protected() || len(sd.DACL) != 2 {
		t.Fatalf("parsed %+v", sd)
	}
	if a := sd.DACL[1]; a.Type != Deny || a.Flags != ContainerInherit|InheritOnly || a.Mask != WriteDAC || a.SID != alice {
		t.Fatalf("second ace %+v", a)
	}
	for _, bad := range []string{"X:BA", "O:", "D:(A;;FA;;)", "D:(A;ZZ;FA;;;WD)", "D:(A;;QQ;;;WD)", "D:(A;;FA;;;WD"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) succeeded", bad)
		}
	}
}

func TestRights(t *testing.T) {
	a := Principal{User: alice, Groups: []string{finance}, Domain: domain}
	b := Principal{User: bob, Domain: domain}
	for _, tc := range []struct {
		name string
		sddl string
		who  Principal
		want uint32
		ok   bool
	}{
		{"no dacl grants everything", "O:BA", b, FileAllAccess, true},
		{"empty dacl grants nothing", "O:BAD:", b, ReadData, false},
		{"empty dacl, the owner can still read and change permissions", "O:" + bob + "D:", b, ReadControl | WriteDAC, true},
		{"empty dacl, the owner can't read data", "O:" + bob + "D:", b, ReadData, false},
		{"group allow", "D:(A;;FR;;;" + finance + ")", a, FileGenericRead, true},
		{"group allow, not a member", "D:(A;;FR;;;" + finance + ")", b, FileGenericRead, false},
		{"everyone", "D:(A;;FR;;;WD)", b, FileGenericRead, true},
		{"authenticated users", "D:(A;;FR;;;AU)", b, FileGenericRead, true},
		{"deny before allow wins", "D:(D;;FW;;;" + alice + ")(A;;FA;;;" + finance + ")", a, FileGenericWrite, false},
		// Explorer's "Deny Write" is 0x116, without the READ_CONTROL and
		// SYNCHRONIZE bits FW shares with FR — denying FW denies reading too.
		{"deny before allow leaves the rest", "D:(D;;0x116;;;" + alice + ")(A;;FA;;;" + finance + ")", a, FileGenericRead, true},
		{"denying FW denies reading too, as on Windows", "D:(D;;FW;;;" + alice + ")(A;;FA;;;" + finance + ")", a, FileGenericRead, false},
		{"allow before deny wins, as Windows does for a non-canonical DACL", "D:(A;;FA;;;" + finance + ")(D;;FW;;;" + alice + ")", a, FileGenericWrite, true},
		{"inherit-only entries don't apply", "D:(A;OICIIO;FA;;;" + alice + ")", a, ReadData, false},
		{"generic all", "D:(A;;GA;;;" + alice + ")", a, FileAllAccess, true},
		{"generic read asked for", "D:(A;;FR;;;" + alice + ")", a, GenericRead, true},
		{"domain alias", "D:(A;;FR;;;DU)", Principal{User: bob, Groups: []string{domain + "-513"}, Domain: domain}, FileGenericRead, true},
		{"domain alias without a domain matches nothing", "D:(A;;FR;;;DU)", Principal{User: bob, Groups: []string{domain + "-513"}}, FileGenericRead, false},
		{"owner rights replace the owner's implicit rights", "O:" + bob + "D:(A;;FR;;;OW)", b, WriteDAC, false},
		{"owner rights grant the owner", "O:" + bob + "D:(A;;FR;;;OW)", b, FileGenericRead, true},
		{"owner rights aren't for others", "O:" + bob + "D:(A;;FR;;;OW)", a, FileGenericRead, false},
		{"audit entries don't grant", "D:(AU;SA;FA;;;WD)", b, ReadData, false},
	} {
		sd := mustParse(t, tc.sddl)
		if got := sd.Allows(tc.who, tc.want); got != tc.ok {
			t.Errorf("%s: Allows = %v, want %v (rights %#x)", tc.name, got, tc.ok, sd.Rights(tc.who))
		}
	}
}

func TestInherit(t *testing.T) {
	parent := mustParse(t, "O:BAG:BAD:PAI(A;OICI;FA;;;BA)(A;CI;0x1200a9;;;"+finance+")(A;OI;FR;;;WD)(A;OICIIO;GA;;;CO)(A;CINP;FW;;;"+bob+")(A;;FA;;;"+alice+")")

	file := Inherit(parent, false, alice, finance)
	if got := file.String(); got != "O:"+alice+"G:"+finance+"D:AI(A;ID;0x1f01ff;;;BA)(A;ID;0x120089;;;WD)(A;ID;0x1f01ff;;;"+alice+")" {
		t.Fatalf("file:\n got  %s", got)
	}
	dir := Inherit(parent, true, alice, finance)
	want := "O:" + alice + "G:" + finance + "D:AI" +
		"(A;OICIID;0x1f01ff;;;BA)" + // carries on as it is
		"(A;CIID;0x1200a9;;;" + finance + ")" + // directories only
		"(A;OIIOID;0x120089;;;WD)" + // files only: a template here
		"(A;ID;0x1f01ff;;;" + alice + ")(A;OICIIOID;0x10000000;;;CO)" + // creator owner: the owner here, the template below
		"(A;ID;0x120116;;;" + bob + ")" // no-propagate: here, not below
	if got := dir.String(); got != want {
		t.Fatalf("dir:\n got  %s\n want %s", got, want)
	}
	// The not-inheritable alice entry wasn't inherited; alice owns the new
	// file and so has full control from the creator-owner template.
	if !file.Allows(Principal{User: alice}, FileAllAccess) {
		t.Fatal("the creator doesn't have full control of their new file")
	}
	if Inherit(nil, false, alice, "").String() != "O:"+alice+"D:AI(A;;0x1f01ff;;;"+alice+")" {
		t.Fatalf("no parent: %s", Inherit(nil, false, alice, ""))
	}
}

func TestPOSIX(t *testing.T) {
	u := POSIXUser{Uid: 1000, Gids: []uint32{100}}
	for _, tc := range []struct {
		mode, uid, gid uint32
		dir            bool
		who            POSIXUser
		want           uint32
		ok             bool
	}{
		{0o640, 1000, 100, false, u, PosixRead | PosixWrite, true},
		{0o640, 2000, 100, false, u, PosixRead, true},
		{0o640, 2000, 100, false, u, PosixWrite, false},
		{0o604, 2000, 200, false, u, PosixRead, true},
		{0o060, 1000, 100, false, u, PosixRead, false}, // the owner's bits apply to the owner, even if the group's are wider
		{0o000, 1000, 100, false, POSIXUser{Uid: 0}, PosixRead | PosixWrite, true},
		{0o644, 1000, 100, false, POSIXUser{Uid: 0}, PosixExec, false},
		{0o755, 1000, 100, false, POSIXUser{Uid: 0}, PosixExec, true},
		{0o700, 1000, 100, true, POSIXUser{Uid: 0}, PosixExec, true},
	} {
		if got := POSIXAllows(tc.mode, tc.uid, tc.gid, tc.dir, tc.who, tc.want); got != tc.ok {
			t.Errorf("mode %o uid %d gid %d dir %v, %+v wants %o: %v, want %v", tc.mode, tc.uid, tc.gid, tc.dir, tc.who, tc.want, got, tc.ok)
		}
	}
}
