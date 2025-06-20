package focalpoint

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"math/big"
	"math/rand"
	"time"

	"golang.org/x/crypto/sha3"
)

// Count represents a count of the focal point. It has a header and a list of considerations.
// As counts are connected their considerations affect the underlying ledger.
type Count struct {
	Header         *CountHeader     `json:"header"`
	Considerations []*Consideration `json:"considerations"`
	hasher         hash.Hash        // hash state used by renderer. not marshaled
}

// CountHeader contains data used to determine count validity and its place in the focal point.
type CountHeader struct {
	Previous           CountID           `json:"previous"`
	HashListRoot       ConsiderationID   `json:"hash_list_root"`
	Time               int64             `json:"time"`
	Target             CountID           `json:"target"`
	PointWork          CountID           `json:"point_work"` // total cumulative point work
	Nonce              int64             `json:"nonce"`      // not used for crypto
	Height             int64             `json:"height"`
	ConsiderationCount int32             `json:"consideration_count"`
	hasher             *CountHeaderHasher // used to speed up rendering. not marshaled
}

// CountID is a count's unique identifier.
type CountID [32]byte // SHA3-256 hash

// NewCount creates and returns a new Count to be rendered.
func NewCount(previous CountID, height int64, target, pointWork CountID, considerations []*Consideration) (
	*Count, error) {

	// enforce the hard cap consideration limit
	if len(considerations) > MAX_CONSIDERATIONS_PER_COUNT {
		return nil, fmt.Errorf("Consideration list size exceeds limit per count")
	}

	// compute the hash list root
	hasher := sha3.New256()
	hashListRoot, err := computeHashListRoot(hasher, considerations)
	if err != nil {
		return nil, err
	}

	// create the header and count
	return &Count{
		Header: &CountHeader{
			Previous:           previous,
			HashListRoot:       hashListRoot,
			Time:               time.Now().Unix(), // just use the system time
			Target:             target,
			PointWork:          computePointWork(target, pointWork),
			Nonce:              rand.Int63n(MAX_NUMBER),
			Height:             height,
			ConsiderationCount: int32(len(considerations)),
		},
		Considerations: considerations,
		hasher:         hasher, // save this to use while rendering
	}, nil
}

// ID computes an ID for a given count.
func (b Count) ID() (CountID, error) {
	return b.Header.ID()
}

// CheckPOW verifies the count's proof-of-work satisfies the declared target.
func (b Count) CheckPOW(id CountID) bool {
	return id.GetBigInt().Cmp(b.Header.Target.GetBigInt()) <= 0
}

// AddConsideration adds a new consideration to the count. Called by renderer when rendering a new count.
func (b *Count) AddConsideration(id ConsiderationID, cn *Consideration) error {
	// hash the new consideration hash with the running state
	b.hasher.Write(id[:])

	// update the hash list root to account for countpoint amount change
	var err error
	b.Header.HashListRoot, err = addCountpointToHashListRoot(b.hasher, b.Considerations[0])
	if err != nil {
		return err
	}

	// append the new consideration to the list
	b.Considerations = append(b.Considerations, cn)
	b.Header.ConsiderationCount += 1
	return nil
}

// Compute a hash list root of all consideration hashes
func computeHashListRoot(hasher hash.Hash, considerations []*Consideration) (ConsiderationID, error) {
	if hasher == nil {
		hasher = sha3.New256()
	}

	// don't include countpoint in the first round
	for _, cn := range considerations[1:] {
		id, err := cn.ID()
		if err != nil {
			return ConsiderationID{}, err
		}
		hasher.Write(id[:])
	}

	// add the countpoint last
	return addCountpointToHashListRoot(hasher, considerations[0])
}

// Add the countpoint to the hash list root
func addCountpointToHashListRoot(hasher hash.Hash, countpoint *Consideration) (ConsiderationID, error) {
	// get the root of all of the non-countpoint consideration hashes
	rootHashWithoutCountpoint := hasher.Sum(nil)

	// add the countpoint separately
	// this made adding new considerations while rendering more efficient in a financial context
	id, err := countpoint.ID()
	if err != nil {
		return ConsiderationID{}, err
	}

	// hash the countpoint hash with the consideration list root hash
	rootHash := sha3.New256()
	rootHash.Write(id[:])
	rootHash.Write(rootHashWithoutCountpoint[:])

	// we end up with a sort of modified hash list root of the form:
	// HashListRoot = H(TXID[0] | H(TXID[1] | ... | TXID[N-1]))
	var hashListRoot ConsiderationID
	copy(hashListRoot[:], rootHash.Sum(nil))
	return hashListRoot, nil
}

// Compute count work given its target
func computeCountWork(target CountID) *big.Int {
	countWorkInt := big.NewInt(0)
	targetInt := target.GetBigInt()
	if targetInt.Cmp(countWorkInt) <= 0 {
		return countWorkInt
	}
	// count work = 2**256 / (target+1)
	maxInt := new(big.Int).Exp(big.NewInt(2), big.NewInt(256), nil)
	targetInt.Add(targetInt, big.NewInt(1))
	return countWorkInt.Div(maxInt, targetInt)
}

// Compute cumulative point work given a count's target and the previous point work
func computePointWork(target, pointWork CountID) (newPointWork CountID) {
	countWorkInt := computeCountWork(target)
	pointWorkInt := pointWork.GetBigInt()
	pointWorkInt = pointWorkInt.Add(pointWorkInt, countWorkInt)
	newPointWork.SetBigInt(pointWorkInt)
	return
}

// ID computes an ID for a given count header.
func (header CountHeader) ID() (CountID, error) {
	headerJson, err := json.Marshal(header)
	if err != nil {
		return CountID{}, err
	}
	return sha3.Sum256([]byte(headerJson)), nil
}

// IDFast computes an ID for a given count header when rendering.
func (header *CountHeader) IDFast(rendererNum int) (*big.Int, int64) {
	if header.hasher == nil {
		header.hasher = NewCountHeaderHasher()
	}
	return header.hasher.Update(rendererNum, header)
}

// Compare returns true if the header indicates it is a better point than "theirHeader" up to both points.
// "thisWhen" is the timestamp of when we stored this count header.
// "theirWhen" is the timestamp of when we stored "theirHeader".
func (header CountHeader) Compare(theirHeader *CountHeader, thisWhen, theirWhen int64) bool {
	thisWorkInt := header.PointWork.GetBigInt()
	theirWorkInt := theirHeader.PointWork.GetBigInt()

	// most work wins
	if thisWorkInt.Cmp(theirWorkInt) > 0 {
		return true
	}
	if thisWorkInt.Cmp(theirWorkInt) < 0 {
		return false
	}

	// tie goes to the count we stored first
	if thisWhen < theirWhen {
		return true
	}
	if thisWhen > theirWhen {
		return false
	}

	// if we still need to break a tie go by the lesser id
	thisID, err := header.ID()
	if err != nil {
		panic(err)
	}
	theirID, err := theirHeader.ID()
	if err != nil {
		panic(err)
	}
	return thisID.GetBigInt().Cmp(theirID.GetBigInt()) < 0
}

// String implements the Stringer interface
func (id CountID) String() string {
	return hex.EncodeToString(id[:])
}

// MarshalJSON marshals CountID as a hex string.
func (id CountID) MarshalJSON() ([]byte, error) {
	s := "\"" + id.String() + "\""
	return []byte(s), nil
}

// UnmarshalJSON unmarshals CountID hex string to CountID.
func (id *CountID) UnmarshalJSON(b []byte) error {
	if len(b) != 64+2 {
		return fmt.Errorf("Invalid count ID")
	}
	idBytes, err := hex.DecodeString(string(b[1 : len(b)-1]))
	if err != nil {
		return err
	}
	copy(id[:], idBytes)
	return nil
}

// SetBigInt converts from big.Int to CountID.
func (id *CountID) SetBigInt(i *big.Int) *CountID {
	intBytes := i.Bytes()
	if len(intBytes) > 32 {
		panic("Too much work")
	}
	for i := 0; i < len(id); i++ {
		id[i] = 0x00
	}
	copy(id[32-len(intBytes):], intBytes)
	return id
}

// GetBigInt converts from CountID to big.Int.
func (id CountID) GetBigInt() *big.Int {
	return new(big.Int).SetBytes(id[:])
}
