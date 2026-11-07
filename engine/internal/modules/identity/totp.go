package identity

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"time"
)

// Time-based one-time codes (RFC 6238 over RFC 4226): HMAC-SHA1, 30-second
// steps, six digits -- what every authenticator app speaks.

const (
	totpPeriod = 30
	totpDigits = 6
	// totpSkew accepts the step before and after now: a phone's clock a few
	// seconds off, or a code typed as it rolled over.
	totpSkew = 1
)

// totpAt is the code for one time step.
func totpAt(secret []byte, step int64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(step))
	m := hmac.New(sha1.New, secret)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// totpCheck reports the step code matches at now, within the skew, and only
// for a step later than last: a code that was accepted once is never
// accepted again, so one read over a shoulder or out of a log is spent.
func totpCheck(secret []byte, code string, now time.Time, last int64) (int64, bool) {
	if len(code) != totpDigits {
		return 0, false
	}
	cur := now.Unix() / totpPeriod
	found, ok := int64(0), false
	for d := int64(-totpSkew); d <= totpSkew; d++ {
		step := cur + d
		// Every candidate is computed and compared, matched or not, so the
		// time taken says nothing about which step (if any) matched.
		if subtle.ConstantTimeCompare([]byte(totpAt(secret, step, totpDigits)), []byte(code)) == 1 && step > last && !ok {
			found, ok = step, true
		}
	}
	return found, ok
}
