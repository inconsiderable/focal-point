package focalpoint

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ed25519"
)

// create a deterministic test count
func makeTestCount(n int) (*Count, error) {
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

	// create the count
	targetBytes, err := hex.DecodeString(INITIAL_TARGET)
	if err != nil {
		return nil, err
	}
	var target CountID
	copy(target[:], targetBytes)
	count, err := NewCount(CountID{}, 0, target, CountID{}, cns)
	if err != nil {
		return nil, err
	}
	return count, nil
}

func TestCountHeaderHasher(t *testing.T) {
	count, err := makeTestCount(10)
	if err != nil {
		t.Fatal(err)
	}

	if !compareIDs(count) {
		t.Fatal("ID mismatch 1")
	}

	count.Header.Time = 1234

	if !compareIDs(count) {
		t.Fatal("ID mismatch 2")
	}

	count.Header.Nonce = 1234

	if !compareIDs(count) {
		t.Fatal("ID mismatch 3")
	}

	count.Header.Nonce = 1235

	if !compareIDs(count) {
		t.Fatal("ID mismatch 4")
	}

	count.Header.Nonce = 1236
	count.Header.Time = 1234

	if !compareIDs(count) {
		t.Fatal("ID mismatch 5")
	}

	count.Header.Time = 123498
	count.Header.Nonce = 12370910

	cnID, _ := count.Considerations[0].ID()
	if err := count.AddConsideration(cnID, count.Considerations[0]); err != nil {
		t.Fatal(err)
	}

	if !compareIDs(count) {
		t.Fatal("ID mismatch 6")
	}

	count.Header.Time = 987654321

	if !compareIDs(count) {
		t.Fatal("ID mismatch 7")
	}
}

func compareIDs(count *Count) bool {
	// compute header ID
	id, _ := count.ID()

	// use delta method
	idInt, _ := count.Header.IDFast(0)
	id2 := new(CountID).SetBigInt(idInt)
	return id == *id2
}
