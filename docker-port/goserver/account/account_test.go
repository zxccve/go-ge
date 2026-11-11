package account

import "testing"

func TestValidateMemberIDAcceptsTheShapesWeUse(t *testing.T) {
	for _, ok := range []string{"arcanine", "test01", "90000000000000001", "Arcanine", "a.b-c_d"} {
		if err := ValidateMemberID(ok); err != nil {
			t.Errorf("ValidateMemberID(%q) = %v, want ok", ok, err)
		}
	}
}

func TestValidateMemberIDRefusesWhatBreaksTheLoginOrTheIni(t *testing.T) {
	cases := map[string]string{
		"":                 "empty",
		" arcanine":        "whitespace",
		"arcanine ":        "whitespace",
		"ar canine":        "space inside",
		"arcanine:test":    "a colon splits accounts.ini",
		"arcanine=test":    "an equals splits accounts.ini",
		"arcanine\ttest":   "a tab",
		"arcanine\x00test": "a non-printable",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "over 64 bytes",
	}
	for in, why := range cases {
		if err := ValidateMemberID(in); err == nil {
			t.Errorf("ValidateMemberID(%q) was accepted, want refused (%s)", in, why)
		}
	}
}

// TestPassMD5IsTheEnginesOwnScheme pins the format to the ORIGINAL engine's, measured on
// 2026-11-11: RESOURCE.ACCOUNT_INFO.PASS is 32 lowercase hex chars for 504 of its 507
// rows, and e10adc3949ba59abbe56e057f20f883e - md5("123456") - is one of them. So the
// column is unsalted md5 of the plaintext and nothing else.
func TestPassMD5IsTheEnginesOwnScheme(t *testing.T) {
	for pass, want := range map[string]string{
		"123456": "e10adc3949ba59abbe56e057f20f883e", // read out of the oracle
		"00":     "b4b147bc522828731f1a016bfa72c073",
		"11":     "6512bd43d9caa6e02c990b0a82652dca",
		"":       "-", // no password set
	} {
		if got := PassMD5(pass); got != want {
			t.Errorf("PassMD5(%q) = %q, want %q", pass, got, want)
		}
	}
}
