package focalpoint

import (
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/ed25519"
)

func TestEncodeCountHeader(t *testing.T) {
	pubKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	// create a countpoint
	cn := NewConsideration(nil, pubKey, 0, 0, 0, "hello")

	// create a count
	targetBytes, err := hex.DecodeString(INITIAL_TARGET)
	if err != nil {
		t.Fatal(err)
	}
	var target CountID
	copy(target[:], targetBytes)
	count, err := NewCount(CountID{}, 0, target, CountID{}, []*Consideration{cn})
	if err != nil {
		t.Fatal(err)
	}

	// encode the header
	encodedHeader, err := encodeCountHeader(count.Header, 12345)
	if err != nil {
		t.Fatal(err)
	}

	// decode the header
	header, when, err := decodeCountHeader(encodedHeader)
	if err != nil {
		t.Fatal(err)
	}

	// compare
	if *header != *count.Header {
		t.Fatal("Decoded header doesn't match original")
	}

	if when != 12345 {
		t.Fatal("Decoded timestamp doesn't match original")
	}
}
