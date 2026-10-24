//go:build windows

package winfspserver

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TestMergeSetsACLControlFlagsAsNTFS: a DACL set through SetSecurity gets
// the protected and auto-inherited flags (SDDL's P and AI) NTFS gives it,
// as measured with SetFileSecurity on the Windows 11 test VM: P from the
// new DACL or PROTECTED_, kept until UNPROTECTED_; AI only when
// inheritance is asked for (AR, as SetSecurityInfo and robocopy /SEC
// send it).
func TestMergeSetsACLControlFlagsAsNTFS(t *testing.T) {
	for _, c := range []struct {
		base, desc string
		info       windows.SECURITY_INFORMATION
		want       string
	}{
		{"O:BAG:SYD:AI(A;ID;FA;;;BA)", "D:AI(A;ID;FA;;;SY)", 0, "O:BAG:SYD:(A;ID;FA;;;SY)"},
		{"O:BAG:SYD:(A;;FA;;;BA)", "D:PAI(A;;FA;;;SY)", 0, "O:BAG:SYD:P(A;;FA;;;SY)"},
		{"O:BAG:SYD:P(A;;FA;;;BA)", "D:(A;;FA;;;SY)", 0, "O:BAG:SYD:P(A;;FA;;;SY)"},
		{"O:BAG:SYD:(A;;FA;;;BA)", "D:ARAI(A;ID;FA;;;SY)", 0, "O:BAG:SYD:AI(A;ID;FA;;;SY)"},
		{"O:BAG:SYD:(A;;FA;;;BA)", "D:(A;;FA;;;SY)", windows.PROTECTED_DACL_SECURITY_INFORMATION, "O:BAG:SYD:P(A;;FA;;;SY)"},
		{"O:BAG:SYD:P(A;;FA;;;BA)", "D:P(A;;FA;;;SY)", windows.UNPROTECTED_DACL_SECURITY_INFORMATION, "O:BAG:SYD:(A;;FA;;;SY)"},
	} {
		base, err := windows.SecurityDescriptorFromString(c.base)
		if err != nil {
			t.Fatal(err)
		}
		desc, err := windows.SecurityDescriptorFromString(c.desc)
		if err != nil {
			t.Fatal(err)
		}
		merged, err := mergeSecurityDescriptor(base, windows.DACL_SECURITY_INFORMATION|c.info, desc)
		if err != nil {
			t.Fatal(err)
		}
		if got := merged.String(); got != c.want {
			t.Errorf("%s onto %s: merged %s, want %s", c.desc, c.base, got, c.want)
		}
	}
}

// TestCreateSDDLMarksInheritance: what WinFsp gives a new item from its
// folder's ACL, unmarked, is stored marked, as NTFS stores it; a
// descriptor the creator supplied, or a protected one, is kept as given.
func TestCreateSDDLMarksInheritance(t *testing.T) {
	parent, err := windows.SecurityDescriptorFromString("O:BAG:SYD:PAI(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		in    string
		isDir bool
		want  string
	}{
		{"O:BAG:SYD:(A;;FA;;;BA)(A;;FA;;;SY)(A;;0x1200a9;;;BU)", false, "O:BAG:SYD:AI(A;ID;FA;;;BA)(A;ID;FA;;;SY)(A;ID;0x1200a9;;;BU)"},
		{"O:BAG:SYD:(A;OICI;FA;;;BA)(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)", true, "O:BAG:SYD:AI(A;OICIID;FA;;;BA)(A;OICIID;FA;;;SY)(A;OICIID;0x1200a9;;;BU)"},
		{"O:BAG:SYD:(A;;FA;;;BA)", false, "O:BAG:SYD:(A;;FA;;;BA)"},
		{"O:BAG:SYD:P(A;;FA;;;BA)(A;;FA;;;SY)(A;;0x1200a9;;;BU)", false, "O:BAG:SYD:P(A;;FA;;;BA)(A;;FA;;;SY)(A;;0x1200a9;;;BU)"},
	} {
		sd, err := windows.SecurityDescriptorFromString(c.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(createSDDL(sd, parent, c.isDir)); got != c.want {
			t.Errorf("%s: stored %s, want %s", c.in, got, c.want)
		}
	}
}

// TestInheritOnlyEntriesKeepGenericRights: as on NTFS, an inherit-only
// entry's generic rights stay generic (they're mapped where inherited);
// an effective entry's are mapped to the file rights.
func TestInheritOnlyEntriesKeepGenericRights(t *testing.T) {
	sd, err := windows.SecurityDescriptorFromString("O:BAG:SYD:(A;OICIIO;GA;;;CO)(A;;GR;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	if err := mapGenericFileRights(sd); err != nil {
		t.Fatal(err)
	}
	if got, want := sd.String(), "O:BAG:SYD:(A;OICIIO;GA;;;CO)(A;;FR;;;BU)"; got != want {
		t.Fatalf("mapped %s, want %s", got, want)
	}
}
