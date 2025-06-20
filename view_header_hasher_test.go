package focalpoint

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ed25519"
)

// create a deterministic test view
func makeTestView(n int) (*View, error) {
	cns := make([]*Consideration, n)

	// create cns
	for i := 0; i < n; i++ {
		// create a sender
		seed := strings.Repeat(strconv.Itoa(i%10), ed25519.SeedSize)
		privKey := ed25519.NewKeyFromSeed([]byte(seed))
		pubKey := privKey.Public().(ed25519.PublicKey)

		// create a recipient
		seed2 := strings.Repeat(strconv.Itoa((i+1)%10), ed25519.SeedSize)
		privKey2 := ed25519.NewKeyFromSeed([]byte(seed2))
		pubKey2 := privKey2.Public().(ed25519.PublicKey)

		matures := MAX_NUMBER
		expires := MAX_NUMBER
		height := MAX_NUMBER

		cn := NewConsideration(pubKey, pubKey2, matures, height, expires, "こんにちは")
		if len(cn.Memo) != 15 {
			// make sure len() gives us bytes not rune count
			return nil, fmt.Errorf("Expected memo length to be 15 but received %d", len(cn.Memo))
		}
		cn.Nonce = int32(123456789 + i)

		// sign the consideration
		if err := cn.Sign(privKey); err != nil {
			return nil, err
		}
		cns[i] = cn
	}

	// create the view
	targetBytes, err := hex.DecodeString(INITIAL_TARGET)
	if err != nil {
		return nil, err
	}
	var target ViewID
	copy(target[:], targetBytes)
	view, err := NewView(ViewID{}, 0, target, ViewID{}, cns)
	if err != nil {
		return nil, err
	}
	return view, nil
}

func TestViewHeaderHasher(t *testing.T) {
	view, err := makeTestView(10)
	if err != nil {
		t.Fatal(err)
	}

	if !compareIDs(view) {
		t.Fatal("ID mismatch 1")
	}

	view.Header.Time = 1234

	if !compareIDs(view) {
		t.Fatal("ID mismatch 2")
	}

	view.Header.Nonce = 1234

	if !compareIDs(view) {
		t.Fatal("ID mismatch 3")
	}

	view.Header.Nonce = 1235

	if !compareIDs(view) {
		t.Fatal("ID mismatch 4")
	}

	view.Header.Nonce = 1236
	view.Header.Time = 1234

	if !compareIDs(view) {
		t.Fatal("ID mismatch 5")
	}

	view.Header.Time = 123498
	view.Header.Nonce = 12370910

	cnID, _ := view.Considerations[0].ID()
	if err := view.AddConsideration(cnID, view.Considerations[0]); err != nil {
		t.Fatal(err)
	}

	if !compareIDs(view) {
		t.Fatal("ID mismatch 6")
	}

	view.Header.Time = 987654321

	if !compareIDs(view) {
		t.Fatal("ID mismatch 7")
	}
}

func compareIDs(view *View) bool {
	// compute header ID
	id, _ := view.ID()

	// use delta method
	idInt, _ := view.Header.IDFast(0)
	id2 := new(ViewID).SetBigInt(idInt)
	return id == *id2
}
