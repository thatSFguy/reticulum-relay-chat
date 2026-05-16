//go:build ignore

// Helper: build a representative ADV (matching the live failure shape:
// 2 parts, t=544, d=476, encrypted) and dump the msgpack body to
// stdout. Run with `go run dump_adv.go > adv_live.bin` then attempt to
// decode in Python with the upstream RNS umsgpack to confirm wire compat.
package main

import (
	"bytes"
	"crypto/sha256"
	"os"

	"github.com/thatSFguy/reticulum-forwarding-service/internal/rns"
)

func main() {
	r := []byte{0xAA, 0xBB, 0xCC, 0xDD}
	body := bytes.Repeat([]byte{0x42}, 476)
	hash := sha256.Sum256(append(append([]byte(nil), body...), r...))

	mh1 := []byte{0x01, 0x02, 0x03, 0x04}
	mh2 := []byte{0x05, 0x06, 0x07, 0x08}

	adv := &rns.ResourceAdvertisement{
		TransferSize:  544,
		DataSize:      476,
		NumParts:      2,
		Hash:          hash[:],
		RandomHash:    r,
		OriginalHash:  hash[:],
		SegmentIndex:  1,
		TotalSegments: 1,
		Flags:         int(rns.ResourceFlagEncrypted),
		Hashmap:       append(mh1, mh2...),
	}
	out, err := rns.PackResourceAdv(adv)
	if err != nil {
		panic(err)
	}
	os.Stdout.Write(out)
}
