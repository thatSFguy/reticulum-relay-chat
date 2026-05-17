package rns

import (
	"bytes"
	"testing"
)

// TestHDLCOversizedFrameResync verifies the audit-A5 cap: a frame whose
// un-escaped payload exceeds maxHDLCFrameSize is discarded, and the
// decoder resyncs to the following frame instead of buffering without
// bound.
func TestHDLCOversizedFrameResync(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(EncodeHDLC(bytes.Repeat([]byte{0x42}, maxHDLCFrameSize+1)))
	stream.Write(EncodeHDLC([]byte("good")))

	dec := NewHDLCDecoder(&stream)
	got, err := dec.NextFrame()
	if err != nil {
		t.Fatalf("decoder failed to resync after an oversized frame: %v", err)
	}
	if string(got) != "good" {
		t.Errorf("after an oversized frame: got %q, want %q", got, "good")
	}
}
