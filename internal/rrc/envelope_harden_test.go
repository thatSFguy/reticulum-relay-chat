package rrc

import (
	"bytes"
	"testing"
)

// TestDecodeRejectsNestingBomb verifies the audit-A4 hardened decoder.
// 24 levels of CBOR map nesting is within the default decoder's limit
// (32) but past the hardened limit (16), so this specifically exercises
// the tightened cap rather than a generic rejection.
func TestDecodeRejectsNestingBomb(t *testing.T) {
	// 0xa1 0x00 = "map with one pair, key 0"; each repetition nests one
	// more map. A trailing 0x00 is the innermost value.
	bomb := append(bytes.Repeat([]byte{0xa1, 0x00}, 24), 0x00)
	if _, err := Decode(bomb); err == nil {
		t.Error("a deeply-nested CBOR document must be rejected by the hardened decoder")
	}
}
