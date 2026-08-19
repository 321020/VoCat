package vowifi

import (
	"strings"
	"testing"
)

func TestResolveCarrierProfileUsesStandardDefault(t *testing.T) {
	profile := ResolveCarrierProfile(SIMIdentity{
		IMSI: "999010000000001", HomeMCC: "999", HomeMNC: "01",
	})
	if profile.ID != CarrierProfileStandard || profile.MatchSource != "standard" {
		t.Fatalf("default profile = %#v", profile)
	}
	if profile.IKEProposal != IKEProposalModern || !profile.AdvertiseEAPOnly ||
		profile.IMSIdentityProfile != IMSProfileStandard || profile.IMSRegisterProfile != IMSProfileStandard {
		t.Fatalf("default profile lost standard capabilities: %#v", profile)
	}
}

func TestResolveCarrierProfilePrefersConstrainedMVNO(t *testing.T) {
	// Cricket MVNO on AT&T network
	cricket := ResolveCarrierProfile(SIMIdentity{
		ICCID: "8901150000000000001", IMSI: "310150000000001",
		HomeMCC: "310", HomeMNC: "150",
	})
	if !strings.Contains(cricket.ID, "cricket") {
		t.Fatalf("Cricket MVNO profile = %#v", cricket)
	}

	// Pure Talk MVNO on AT&T network via GID1
	pureTalk := ResolveCarrierProfile(SIMIdentity{
		IMSI: "310410000000001", HomeMCC: "310", HomeMNC: "410", GID1: "62FFFF",
	})
	if !strings.Contains(pureTalk.ID, "pure-talk") {
		t.Fatalf("Pure Talk MVNO profile = %#v", pureTalk)
	}
}

func TestResolveCarrierProfileUsesAppleGID1Selector(t *testing.T) {
	profile := ResolveCarrierProfile(SIMIdentity{
		IMSI: "234100000000001", HomeMCC: "234", HomeMNC: "10", GID1: "508FFFFF",
	})
	if !strings.Contains(profile.ID, "giffgaff") || profile.MatchSource != "hplmn+gid1" {
		t.Fatalf("giffgaff profile = %#v", profile)
	}
}

func TestResolveCarrierProfileATT(t *testing.T) {
	profile := ResolveCarrierProfile(SIMIdentity{
		ICCID: "8901410000000000001", IMSI: "310410000000001", HomeMCC: "310", HomeMNC: "410",
	})
	if !strings.Contains(profile.ID, "att") {
		t.Fatalf("AT&T profile = %#v", profile)
	}
}

func TestResolveCarrierProfileStandardHasNoRegisterOverrides(t *testing.T) {
	profile := ResolveCarrierProfile(SIMIdentity{HomeMCC: "999", HomeMNC: "99"})
	if profile.ID != CarrierProfileStandard {
		t.Fatalf("profile = %q", profile.ID)
	}
	if profile.IMSRegisterOptions.ExpirySeconds != 0 {
		t.Fatalf("standard expiry = %d", profile.IMSRegisterOptions.ExpirySeconds)
	}
	if profile.IMSRegisterOptions.ContactFormat != "" {
		t.Fatalf("standard contact format = %q", profile.IMSRegisterOptions.ContactFormat)
	}
	if profile.IMSRegisterOptions.SupportedHeader != nil {
		t.Fatalf("standard supported header = %v", *profile.IMSRegisterOptions.SupportedHeader)
	}
	if profile.AllowSMSWithoutContactConfirmation {
		t.Fatal("standard profile should require SMS contact confirmation")
	}
}
