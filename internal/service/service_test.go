package service

import (
	"bytes"
	"testing"
)

// TestParseIdentifyFrame pins the two accepted §6.6 LINKIDENTIFY
// payload layouts — the 128-byte upstream RNS form (public_key||sig,
// what a spec-compliant client such as the Python RRC desktop client
// sends) and the 144-byte legacy form some older reticulum-mobile-app
// builds still send — plus the rejections.
func TestParseIdentifyFrame(t *testing.T) {
	linkID := bytes.Repeat([]byte{0xAB}, 16)
	pub := bytes.Repeat([]byte{0x11}, 64)
	sig := bytes.Repeat([]byte{0x22}, 64)

	t.Run("upstream 128-byte form", func(t *testing.T) {
		frame := concat(pub, sig)
		gotPub, gotSig, ok := parseIdentifyFrame(linkID, frame)
		if !ok {
			t.Fatal("128-byte public_key||sig form must be accepted")
		}
		if !bytes.Equal(gotPub, pub) || !bytes.Equal(gotSig, sig) {
			t.Error("128-byte form sliced wrong")
		}
	})

	t.Run("legacy 144-byte form", func(t *testing.T) {
		frame := concat(linkID, pub, sig)
		gotPub, gotSig, ok := parseIdentifyFrame(linkID, frame)
		if !ok {
			t.Fatal("144-byte link_id||public_key||sig form must be accepted")
		}
		if !bytes.Equal(gotPub, pub) || !bytes.Equal(gotSig, sig) {
			t.Error("144-byte form sliced wrong")
		}
	})

	t.Run("legacy form with a mismatched link_id is rejected", func(t *testing.T) {
		frame := concat(bytes.Repeat([]byte{0xCC}, 16), pub, sig)
		if _, _, ok := parseIdentifyFrame(linkID, frame); ok {
			t.Error("a 144-byte frame whose embedded link_id is wrong must be rejected")
		}
	})

	t.Run("other lengths are rejected", func(t *testing.T) {
		// 80 is fwdsvc's old (also-wrong) form; the rest are near-misses.
		for _, n := range []int{0, 80, 96, 127, 129, 143, 145} {
			if _, _, ok := parseIdentifyFrame(linkID, make([]byte, n)); ok {
				t.Errorf("%d-byte frame must be rejected", n)
			}
		}
	})
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
