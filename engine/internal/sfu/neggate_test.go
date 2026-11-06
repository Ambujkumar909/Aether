package sfu

import "testing"

// The gate's contract, step by step: one offer at a time, and a request made
// while one is out is run when it ends -- answered or failed. The race this
// replaced (a request lost between two atomic flags) was reproduced by the
// 50-person WebRTC stress run: one receiver with 47 of 49 cameras.
func TestNegGateRunsARequestMadeDuringAnOffer(t *testing.T) {
	var g negGate
	if !g.begin() {
		t.Fatal("first request did not start an offer")
	}
	for range 2 {
		if g.begin() {
			t.Fatal("a second offer started while one was out")
		}
	}
	if !g.done() {
		t.Fatal("the request made during the offer was lost")
	}
	if g.done() {
		t.Fatal("two requests during one offer ran two more offers; one covers both")
	}
	if !g.begin() {
		t.Fatal("the gate stayed shut after the last offer ended")
	}
}
