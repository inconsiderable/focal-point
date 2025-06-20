package focalpoint

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"

	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/sha3"
)

// Consideration represents a ledger consideration. It transfers value from one public key to another.
type Consideration struct {//TODO: Beware of reordering struct fields, IDs seem to be sensitive to field order.
	Time      int64             `json:"time"`
	Nonce     int32             `json:"nonce"` // collision prevention. pseudorandom. not used for crypto
	By        ed25519.PublicKey `json:"by,omitempty"`
	For       ed25519.PublicKey `json:"for"`
	Memo      string            `json:"memo,omitempty"`    // max 200 characters
	Matures   int64             `json:"matures,omitempty"` // view height. if set consideration can't be rendered before
	Expires   int64             `json:"expires,omitempty"` // view height. if set consideration can't be rendered after
	Series    int64             `json:"series"`            // +1 roughly once a week to allow for pruning history
	Signature Signature         `json:"signature,omitempty"`
}

// ConsiderationID is a consideration's unique identifier.
type ConsiderationID [32]byte // SHA3-256 hash

// Signature is a consideration's signature.
type Signature []byte

// NewConsideration returns a new unsigned consideration.
func NewConsideration(by, forr ed25519.PublicKey, matures, expires, height int64, memo string) *Consideration {
	return &Consideration{
		Time:    time.Now().Unix(),
		Nonce:   rand.Int31(),
		By:      by,
		For:     forr,
		Memo:    memo,
		Matures: matures,
		Expires: expires,
		Series:  computeConsiderationSeries(by == nil, height),
	}
}

// ID computes an ID for a given consideration.
func (cn Consideration) ID() (ConsiderationID, error) {
	// never include the signature in the ID
	// this way we never have to think about signature malleability
	cn.Signature = nil
	cnJson, err := json.Marshal(cn)
	if err != nil {
		return ConsiderationID{}, err
	}
	return sha3.Sum256([]byte(cnJson)), nil
}

// Sign is called to sign a consideration.
func (cn *Consideration) Sign(privKey ed25519.PrivateKey) error {
	id, err := cn.ID()
	if err != nil {
		return err
	}
	cn.Signature = ed25519.Sign(privKey, id[:])
	return nil
}

// Verify is called to verify only that the consideration is properly signed.
func (cn Consideration) Verify() (bool, error) {
	id, err := cn.ID()
	if err != nil {
		return false, err
	}
	return ed25519.Verify(cn.By, id[:], cn.Signature), nil
}

// IsViewpoint returns true if the consideration is a viewpoint. A viewpoint is the first consideration in every view
// used to recognise the renderer for rendering the view.
func (cn Consideration) IsViewpoint() bool {
	return cn.By == nil
}

// Contains returns true if the consideration is relevant to the given public key.
func (cn Consideration) Contains(pubKey ed25519.PublicKey) bool {
	if !cn.IsViewpoint() {
		if bytes.Equal(pubKey, cn.By) {
			return true
		}
	}
	return bytes.Equal(pubKey, cn.For)
}

// IsMature returns true if the consideration can be rendered at the given height.
func (cn Consideration) IsMature(height int64) bool {
	if cn.Matures == 0 {
		return true
	}
	return cn.Matures >= height
}

// IsExpired returns true if the consideration cannot be rendered at the given height.
func (cn Consideration) IsExpired(height int64) bool {
	if cn.Expires == 0 {
		return false
	}
	return cn.Expires < height
}

// String implements the Stringer interface.
func (id ConsiderationID) String() string {
	return hex.EncodeToString(id[:])
}

// MarshalJSON marshals ConsiderationID as a hex string.
func (id ConsiderationID) MarshalJSON() ([]byte, error) {
	s := "\"" + id.String() + "\""
	return []byte(s), nil
}

// UnmarshalJSON unmarshals a hex string to ConsiderationID.
func (id *ConsiderationID) UnmarshalJSON(b []byte) error {
	if len(b) != 64+2 {
		return fmt.Errorf("Invalid consideration ID")
	}
	idBytes, err := hex.DecodeString(string(b[1 : len(b)-1]))
	if err != nil {
		return err
	}
	copy(id[:], idBytes)
	return nil
}

// Compute the series to use for a new consideration.
func computeConsiderationSeries(isViewpoint bool, height int64) int64 {
	if isViewpoint {
		// viewpoints start using the new series right on time
		return height/VIEWS_UNTIL_NEW_SERIES + 1
	}

	// otherwise don't start using a new series until 100 views in to mitigate
	// potential reorg issues right around the switchover
	return (height-100)/VIEWS_UNTIL_NEW_SERIES + 1
}
