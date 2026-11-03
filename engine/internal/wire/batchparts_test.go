package wire

import (
	"bytes"
	"testing"
)

// A frame in two parts is the same frame on the wire as one in a piece,
// alone or in a batch, and a shared tail is not changed by being sent.
func TestBatchPartsAreFramesWhole(t *testing.T) {
	shared := []byte("one payload for the whole room")
	heads := [][]byte{{0x45, 1}, {0x46}, {0x47, 3, 4}}
	tails := [][]byte{shared, nil, shared}
	w := NewWriter(nil)
	if err := EncodeBatchParts(w, heads, tails); err != nil {
		t.Fatal(err)
	}
	var whole [][]byte
	for i, h := range heads {
		whole = append(whole, append(append([]byte(nil), h...), tails[i]...))
	}
	w2 := NewWriter(nil)
	if err := EncodeBatch(w2, whole); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Bytes(), w2.Bytes()) {
		t.Fatalf("two-part batch differs from the whole one:\n%x\n%x", w.Bytes(), w2.Bytes())
	}
	// Alone: head then tail, no container.
	w3 := NewWriter(nil)
	_ = EncodeBatchParts(w3, heads[:1], tails[:1])
	if !bytes.Equal(w3.Bytes(), whole[0]) {
		t.Fatalf("a lone two-part frame: %x", w3.Bytes())
	}
	if string(shared) != "one payload for the whole room" {
		t.Fatal("the shared payload changed")
	}
}
