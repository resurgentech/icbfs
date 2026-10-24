//go:build windows

package winfspserver

import (
	"testing"

	"golang.org/x/sys/windows"
)

// TestMergeKeepsACLControlFlags: a DACL set through SetSecurity keeps
// its protected and auto-inherited flags (SDDL's P and AI), as robocopy
// /SEC and xcopy /O set them; and SetSecurityInfo's UNPROTECTED_ flag
// takes protection away.
func TestMergeKeepsACLControlFlags(t *testing.T) {
	base, err := windows.SecurityDescriptorFromString("O:BAG:SYD:(A;;FA;;;BA)")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		desc string
		info windows.SECURITY_INFORMATION
		want string
	}{
		{"D:PAI(A;;FA;;;SY)", windows.DACL_SECURITY_INFORMATION, "O:BAG:SYD:PAI(A;;FA;;;SY)"},
		{"D:AI(A;ID;FA;;;SY)", windows.DACL_SECURITY_INFORMATION, "O:BAG:SYD:AI(A;ID;FA;;;SY)"},
		{"D:(A;;FA;;;SY)", windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION, "O:BAG:SYD:P(A;;FA;;;SY)"},
		{"D:P(A;;FA;;;SY)", windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION, "O:BAG:SYD:(A;;FA;;;SY)"},
	} {
		desc, err := windows.SecurityDescriptorFromString(c.desc)
		if err != nil {
			t.Fatal(err)
		}
		merged, err := mergeSecurityDescriptor(base, c.info, desc)
		if err != nil {
			t.Fatal(err)
		}
		if got := merged.String(); got != c.want {
			t.Errorf("%s: merged %s, want %s", c.desc, got, c.want)
		}
	}
}

// TestCreateSDDLMarksInheritance: a new item's descriptor with entries
// from its folder is marked auto-inherited, as NTFS marks it; an explicit
// or protected one is left as given.
func TestCreateSDDLMarksInheritance(t *testing.T) {
	for in, want := range map[string]string{
		"O:BAG:SYD:(A;ID;FA;;;BA)(A;ID;0x1200a9;;;BU)": "O:BAG:SYD:AI(A;ID;FA;;;BA)(A;ID;0x1200a9;;;BU)",
		"O:BAG:SYD:(A;;FA;;;BA)":                       "O:BAG:SYD:(A;;FA;;;BA)",
		"O:BAG:SYD:P(A;ID;FA;;;BA)":                    "O:BAG:SYD:P(A;ID;FA;;;BA)",
	} {
		sd, err := windows.SecurityDescriptorFromString(in)
		if err != nil {
			t.Fatal(err)
		}
		if got := string(createSDDL(sd)); got != want {
			t.Errorf("%s: stored %s, want %s", in, got, want)
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
