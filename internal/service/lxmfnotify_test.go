package service

import (
	"bytes"
	"io"
	"log"
	"testing"

	"github.com/thatSFguy/reticulum-go/rns"
	"github.com/thatSFguy/reticulum-relay-chat/internal/lxmfaddr"
)

// deliveryKnown builds the announce-cache entry the offline-notify path
// synthesizes from a LINKIDENTIFY key: correct destination, no app_data.
func deliveryKnown(t *testing.T) (*rns.Identity, *rns.KnownIdentity) {
	t.Helper()
	id, err := rns.NewIdentity()
	if err != nil {
		t.Fatalf("NewIdentity: %v", err)
	}
	known, err := lxmfaddr.KnownDelivery(id.PublicKey())
	if err != nil {
		t.Fatalf("KnownDelivery: %v", err)
	}
	return id, known
}

// The recipient's stamp_cost lives in the app_data of their announce,
// and a delivery stamp is only ground when that cost can be read. The
// synthesized entry has no app_data, so restoring it over a real
// announce sends an unstamped message — which a recipient enforcing
// stamps drops without a word, while the hub logs a successful upload.
func TestARealAnnounceSurvivesTheSynthesizedEntry(t *testing.T) {
	tr := rns.NewTransport(log.New(io.Discard, "", 0))
	id, known := deliveryKnown(t)

	// What a live lxmf.delivery announce from that peer leaves behind:
	// the same key, plus the app_data carrying its stamp cost.
	announced := *known
	announced.AppData = []byte("announced app_data with a stamp cost")
	announced.Hops = 3
	tr.Restore(&announced)

	ensureAddressable(tr, known)

	got := tr.Recall(known.DestHash)
	if got == nil {
		t.Fatal("destination is not addressable at all")
	}
	if !bytes.Equal(got.AppData, announced.AppData) {
		t.Errorf("app_data is %q, want the announced %q — the stamp cost was lost",
			got.AppData, announced.AppData)
	}
	if got.Hops != announced.Hops {
		t.Errorf("hops = %d, want the announced %d", got.Hops, announced.Hops)
	}
	if !bytes.Equal(got.PublicKey, id.PublicKey()) {
		t.Error("public key changed")
	}
}

// The other half: a peer the hub has never heard announce must still
// become addressable from the key alone. That is the whole point of
// deriving the address from LINKIDENTIFY.
func TestAnUnheardPeerIsStillMadeAddressable(t *testing.T) {
	tr := rns.NewTransport(log.New(io.Discard, "", 0))
	id, known := deliveryKnown(t)

	if tr.Recall(known.DestHash) != nil {
		t.Fatal("setup: the transport already knows this destination")
	}
	ensureAddressable(tr, known)

	got := tr.Recall(known.DestHash)
	if got == nil {
		t.Fatal("peer never became addressable; an offline mention could not be sent")
	}
	if !bytes.Equal(got.PublicKey, id.PublicKey()) {
		t.Error("restored entry carries the wrong public key")
	}
}
