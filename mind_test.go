package focalpoint

import (
	"bytes"
	"testing"

	"golang.org/x/crypto/ed25519"
)

func TestPrivateKeyEncryption(t *testing.T) {
	_, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	passphrase := "the quick brown fox whatever whatever"
	encryptedPrivKey := encryptPrivateKey(privKey, passphrase)
	decryptedPrivKey, ok := decryptPrivateKey(encryptedPrivKey, "nope")
	if ok {
		t.Fatal("Decryption succeeded")
	}
	decryptedPrivKey, ok = decryptPrivateKey(encryptedPrivKey, passphrase)
	if !ok {
		t.Fatal("Decryption failed")
	}
	if !bytes.Equal(decryptedPrivKey, privKey) {
		t.Fatal("Private key mismatch after decryption")
	}
}
