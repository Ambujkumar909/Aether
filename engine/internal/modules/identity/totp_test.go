package identity

import (
	"testing"
	"time"
)

// RFC 6238, Appendix B, the SHA-1 rows: the shared secret is the ASCII
// string "12345678901234567890", eight digits.
func TestTOTPMatchesTheRFCVectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, v := range []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	} {
		if got := totpAt(secret, v.unix/totpPeriod, 8); got != v.want {
			t.Errorf("T=%d: %s, want %s", v.unix, got, v.want)
		}
	}
}

func TestACodeIsAcceptedOnceWithinOneStepEitherSide(t *testing.T) {
	secret := []byte("12345678901234567890")
	now := time.Unix(1234567890, 0)
	step := now.Unix() / totpPeriod
	code := totpAt(secret, step, totpDigits)

	got, ok := totpCheck(secret, code, now, 0)
	if !ok || got != step {
		t.Fatalf("the current code: %d %v", got, ok)
	}
	if _, ok := totpCheck(secret, code, now, got); ok {
		t.Fatal("the same code was accepted twice")
	}
	// A clock 30 s either way.
	if _, ok := totpCheck(secret, totpAt(secret, step-1, totpDigits), now, 0); !ok {
		t.Fatal("the previous step was refused")
	}
	if _, ok := totpCheck(secret, totpAt(secret, step+1, totpDigits), now, 0); !ok {
		t.Fatal("the next step was refused")
	}
	// Further is refused.
	for _, d := range []int64{-3, -2, 2, 3} {
		if _, ok := totpCheck(secret, totpAt(secret, step+d, totpDigits), now, 0); ok {
			t.Errorf("a code %d steps away was accepted", d)
		}
	}
	// An older step than one already used is refused even inside the window.
	if _, ok := totpCheck(secret, totpAt(secret, step-1, totpDigits), now, step); ok {
		t.Fatal("a step before the last used one was accepted")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := totpCheck(secret, bad, now, 0); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}
