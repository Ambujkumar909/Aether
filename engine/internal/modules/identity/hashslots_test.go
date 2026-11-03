package identity

import (
	"runtime"
	"sync"
	"testing"
)

// However many sign-ins arrive at once, no more than min(cores, 8) argon2id
// computations run together: the memory ceiling is slots x 19 MiB, not
// sign-ins x 19 MiB (the 50-person stress run reached 992 MB without it).
func TestPasswordHashingIsBounded(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	hashPeak.Store(0)
	n := 4 * runtime.GOMAXPROCS(0)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !VerifyPassword("correct horse battery staple", h) {
				t.Error("a right password failed")
			}
		}()
	}
	wg.Wait()
	if peak, limit := hashPeak.Load(), int32(min(runtime.GOMAXPROCS(0), maxHashSlots)); peak > limit || peak < 1 {
		t.Fatalf("%d sign-ins at once ran %d hashes together; the bound is %d", n, peak, limit)
	}
	if hashInFlight.Load() != 0 {
		t.Fatalf("%d hashes still counted in flight", hashInFlight.Load())
	}
}
