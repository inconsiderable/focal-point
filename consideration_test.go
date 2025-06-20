package focalpoint

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"golang.org/x/crypto/ed25519"
)

func TestConsideration(t *testing.T) {
	// create a sender
	pubKey, privKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	// create a recipient
	pubKey2, privKey2, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	// create the unsigned consideration
	cn := NewConsideration(pubKey, pubKey2, 0, 0, 0, "for lunch")

	// sign the consideration
	if err := cn.Sign(privKey); err != nil {
		t.Fatal(err)
	}

	// verify the consideration
	ok, err := cn.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("Verification failed")
	}

	// re-sign the consideration with the wrong private key
	if err := cn.Sign(privKey2); err != nil {
		t.Fatal(err)
	}

	// verify the consideration (should fail)
	ok, err = cn.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("Expected verification failure")
	}
}

func TestConsiderationTestVector1(t *testing.T) {
	// create consideration for Test Vector 1
	pubKeyBytes, err := base64.StdEncoding.DecodeString("80tvqyCax0UdXB+TPvAQwre7NxUHhISm/bsEOtbF+yI=")
	if err != nil {
		t.Fatal(err)
	}
	pubKey := ed25519.PublicKey(pubKeyBytes)

	pubKeyBytes2, err := base64.StdEncoding.DecodeString("YkJHRtoQDa1TIKhN7gKCx54bavXouJy4orHwcRntcZY=")
	if err != nil {
		t.Fatal(err)
	}
	pubKey2 := ed25519.PublicKey(pubKeyBytes2)

	cn := NewConsideration(pubKey, pubKey2, 0, 0, 0, "for lunch")
	cn.Time = 1558565474
	cn.Nonce = 2019727887

	// check JSON matches test vector
	cnJson, err := json.Marshal(cn)
	if err != nil {
		t.Fatal(err)
	}
	if string(cnJson) != `{"time":1558565474,"nonce":2019727887,"from":"80tvqyCax0UdXB+TPvAQwre7NxUHhISm/bsEOtbF+yI=",`+
		`"to":"YkJHRtoQDa1TIKhN7gKCx54bavXouJy4orHwcRntcZY=","memo":"for lunch","series":1}` {
		t.Fatal("JSON differs from test vector: " + string(cnJson))
	}

	// check ID matches test vector
	id, err := cn.ID()
	if err != nil {
		t.Fatal(err)
	}
	if id.String() != "04c5193340be556888ef4e1c2bdad865b83b01aa637381a382afbdf1abaedb5f" {
		t.Fatalf("ID %s differs from test vector", id)
	}

	// add signature from test vector
	sigBytes, err := base64.StdEncoding.DecodeString("i3XHtB9CrWFB/B3UBNBFQRZD236NNZjvIBfvFPlKyFccW4BLwZ/xBZyxAzrRfY7TwbzsuMKxh5+oGgxx9FTzDw==")
	if err != nil {
		t.Fatal(err)
	}
	cn.Signature = Signature(sigBytes)

	// verify the consideration
	ok, err := cn.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("Verification failed")
	}

	// re-sign the consideration with private key from test vector
	privKeyBytes, err := base64.StdEncoding.DecodeString("EBQtXb3/Ht6KFh8/+Lxk9aDv2Zrag5G8r+dhElbCe07zS2+rIJrHRR1cH5M+8BDCt7s3FQeEhKb9uwQ61sX7Ig==")
	if err != nil {
		t.Fatal(err)
	}
	privKey := ed25519.PrivateKey(privKeyBytes)
	if err := cn.Sign(privKey); err != nil {
		t.Fatal(err)
	}

	// verify the consideration
	ok, err = cn.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("Verification failed")
	}

	// re-sign the consideration with the wrong private key
	_, privKey2, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cn.Sign(privKey2); err != nil {
		t.Fatal(err)
	}

	// verify the consideration (should fail)
	ok, err = cn.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Errorf("Expected verification failure")
	}
}
