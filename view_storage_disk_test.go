package focalpoint

import (
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/ed25519"
)

func TestEncodeViewHeader(t *testing.T) {
	pubKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	// create a viewpoint
	cn := NewConsideration(nil, pubKey, 0, 0, 0, "hello")

	// create a view
	targetBytes, err := hex.DecodeString(INITIAL_TARGET)
	if err != nil {
		t.Fatal(err)
	}
	var target ViewID
	copy(target[:], targetBytes)
	view, err := NewView(ViewID{}, 0, target, ViewID{}, []*Consideration{cn})
	if err != nil {
		t.Fatal(err)
	}

	// encode the header
	encodedHeader, err := encodeViewHeader(view.Header, 12345)
	if err != nil {
		t.Fatal(err)
	}

	// decode the header
	header, when, err := decodeViewHeader(encodedHeader)
	if err != nil {
		t.Fatal(err)
	}

	// compare
	if *header != *view.Header {
		t.Fatal("Decoded header doesn't match original")
	}

	if when != 12345 {
		t.Fatal("Decoded timestamp doesn't match original")
	}
}
