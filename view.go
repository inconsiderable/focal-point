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

// View represents a view of the focal point. It has a header and a list of considerations.
// As views are connected their considerations affect the underlying ledger.
type View struct {
	Header         *ViewHeader      `json:"header"`
	Considerations []*Consideration `json:"considerations"`
	hasher         hash.Hash        // hash state used by renderer. not marshaled
}

// ViewHeader contains data used to determine view validity and its place in the focal point.
type ViewHeader struct {
	Previous           ViewID            `json:"previous"`
	HashListRoot       ConsiderationID   `json:"hash_list_root"`
	Time               int64             `json:"time"`
	Target             ViewID            `json:"target"`
	PointWork          ViewID            `json:"point_work"` // total cumulative point work
	Nonce              int64             `json:"nonce"`      // not used for crypto
	Height             int64             `json:"height"`
	ConsiderationCount int32             `json:"consideration_count"`
	hasher             *ViewHeaderHasher // used to speed up rendering. not marshaled
}

// ViewID is a view's unique identifier.
type ViewID [32]byte // SHA3-256 hash

// NewView creates and returns a new View to be rendered.
func NewView(previous ViewID, height int64, target, pointWork ViewID, considerations []*Consideration) (
	*View, error) {

	// enforce the hard cap consideration limit
	if len(considerations) > MAX_CONSIDERATIONS_PER_VIEW {
		return nil, fmt.Errorf("Consideration list size exceeds limit per view")
	}

	// compute the hash list root
	hasher := sha3.New256()
	hashListRoot, err := computeHashListRoot(hasher, considerations)
	if err != nil {
		return nil, err
	}

	// create the header and view
	return &View{
		Header: &ViewHeader{
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

// ID computes an ID for a given view.
func (b View) ID() (ViewID, error) {
	return b.Header.ID()
}

// CheckPOW verifies the view's proof-of-work satisfies the declared target.
func (b View) CheckPOW(id ViewID) bool {
	return id.GetBigInt().Cmp(b.Header.Target.GetBigInt()) <= 0
}

// AddConsideration adds a new consideration to the view. Called by renderer when rendering a new view.
func (b *View) AddConsideration(id ConsiderationID, cn *Consideration) error {
	// hash the new consideration hash with the running state
	b.hasher.Write(id[:])

	// update the hash list root to account for viewpoint amount change
	var err error
	b.Header.HashListRoot, err = addViewpointToHashListRoot(b.hasher, b.Considerations[0])
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

	// don't include viewpoint in the first round
	for _, cn := range considerations[1:] {
		id, err := cn.ID()
		if err != nil {
			return ConsiderationID{}, err
		}
		hasher.Write(id[:])
	}

	// add the viewpoint last
	return addViewpointToHashListRoot(hasher, considerations[0])
}

// Add the viewpoint to the hash list root
func addViewpointToHashListRoot(hasher hash.Hash, viewpoint *Consideration) (ConsiderationID, error) {
	// get the root of all of the non-viewpoint consideration hashes
	rootHashWithoutViewpoint := hasher.Sum(nil)

	// add the viewpoint separately
	// this made adding new considerations while rendering more efficient in a financial context
	id, err := viewpoint.ID()
	if err != nil {
		return ConsiderationID{}, err
	}

	// hash the viewpoint hash with the consideration list root hash
	rootHash := sha3.New256()
	rootHash.Write(id[:])
	rootHash.Write(rootHashWithoutViewpoint[:])

	// we end up with a sort of modified hash list root of the form:
	// HashListRoot = H(TXID[0] | H(TXID[1] | ... | TXID[N-1]))
	var hashListRoot ConsiderationID
	copy(hashListRoot[:], rootHash.Sum(nil))
	return hashListRoot, nil
}

// Compute view work given its target
func computeViewWork(target ViewID) *big.Int {
	viewWorkInt := big.NewInt(0)
	targetInt := target.GetBigInt()
	if targetInt.Cmp(viewWorkInt) <= 0 {
		return viewWorkInt
	}
	// view work = 2**256 / (target+1)
	maxInt := new(big.Int).Exp(big.NewInt(2), big.NewInt(256), nil)
	targetInt.Add(targetInt, big.NewInt(1))
	return viewWorkInt.Div(maxInt, targetInt)
}

// Compute cumulative point work given a view's target and the previous point work
func computePointWork(target, pointWork ViewID) (newPointWork ViewID) {
	viewWorkInt := computeViewWork(target)
	pointWorkInt := pointWork.GetBigInt()
	pointWorkInt = pointWorkInt.Add(pointWorkInt, viewWorkInt)
	newPointWork.SetBigInt(pointWorkInt)
	return
}

// ID computes an ID for a given view header.
func (header ViewHeader) ID() (ViewID, error) {
	headerJson, err := json.Marshal(header)
	if err != nil {
		return ViewID{}, err
	}
	return sha3.Sum256([]byte(headerJson)), nil
}

// IDFast computes an ID for a given view header when rendering.
func (header *ViewHeader) IDFast(rendererNum int) (*big.Int, int64) {
	if header.hasher == nil {
		header.hasher = NewViewHeaderHasher()
	}
	return header.hasher.Update(rendererNum, header)
}

// Compare returns true if the header indicates it is a better point than "theirHeader" up to both points.
// "thisWhen" is the timestamp of when we stored this view header.
// "theirWhen" is the timestamp of when we stored "theirHeader".
func (header ViewHeader) Compare(theirHeader *ViewHeader, thisWhen, theirWhen int64) bool {
	thisWorkInt := header.PointWork.GetBigInt()
	theirWorkInt := theirHeader.PointWork.GetBigInt()

	// most work wins
	if thisWorkInt.Cmp(theirWorkInt) > 0 {
		return true
	}
	if thisWorkInt.Cmp(theirWorkInt) < 0 {
		return false
	}

	// tie goes to the view we stored first
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
func (id ViewID) String() string {
	return hex.EncodeToString(id[:])
}

// MarshalJSON marshals ViewID as a hex string.
func (id ViewID) MarshalJSON() ([]byte, error) {
	s := "\"" + id.String() + "\""
	return []byte(s), nil
}

// UnmarshalJSON unmarshals ViewID hex string to ViewID.
func (id *ViewID) UnmarshalJSON(b []byte) error {
	if len(b) != 64+2 {
		return fmt.Errorf("Invalid view ID")
	}
	idBytes, err := hex.DecodeString(string(b[1 : len(b)-1]))
	if err != nil {
		return err
	}
	copy(id[:], idBytes)
	return nil
}

// SetBigInt converts from big.Int to ViewID.
func (id *ViewID) SetBigInt(i *big.Int) *ViewID {
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

// GetBigInt converts from ViewID to big.Int.
func (id ViewID) GetBigInt() *big.Int {
	return new(big.Int).SetBytes(id[:])
}
