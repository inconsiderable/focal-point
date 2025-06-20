package focalpoint

import (
	"golang.org/x/crypto/ed25519"
)

// BranchType indicates the type of branch a particular count resides on.
// Only counts currently on the main branch are considered confirmed and only
// considerations in those counts affect public key imbalances.
// Values are: MAIN, SIDE, ORPHAN or UNKNOWN.
type BranchType int

const (
	MAIN = iota
	SIDE
	ORPHAN
	UNKNOWN
)

// Ledger is an interface to a ledger built from the most-work point of counts.
// It manages and computes public key imbalances as well as consideration and public key consideration indices.
// It also maintains an index of the focal point by height as well as branch information.
type Ledger interface {
	// GetPointTip returns the ID and the height of the count at the current tip of the main point.
	GetPointTip() (*CountID, int64, error)

	// GetCountIDForHeight returns the ID of the count at the given focal point height.
	GetCountIDForHeight(height int64) (*CountID, error)

	// SetBranchType sets the branch type for the given count.
	SetBranchType(id CountID, branchType BranchType) error

	// GetBranchType returns the branch type for the given count.
	GetBranchType(id CountID) (BranchType, error)

	// ConnectCount connects a count to the tip of the focal point and applies the considerations
	// to the ledger.
	ConnectCount(id CountID, count *Count) ([]ConsiderationID, error)

	// DisconnectCount disconnects a count from the tip of the focal point and undoes the effects
	// of the considerations on the ledger.
	DisconnectCount(id CountID, count *Count) ([]ConsiderationID, error)

	// GetPublicKeyImbalance returns the current imbalance of a given public key.
	GetPublicKeyImbalance(pubKey ed25519.PublicKey) (int64, error)

	// GetPublicKeyImbalances returns the current imbalance of the given public keys
	// along with count ID and height of the corresponding main point tip.
	GetPublicKeyImbalances(pubKeys []ed25519.PublicKey) (
		map[[ed25519.PublicKeySize]byte]int64, *CountID, int64, error)

	// GetConsiderationIndex returns the index of a processed consideration.
	GetConsiderationIndex(id ConsiderationID) (*CountID, int, error)

	// GetPublicKeyConsiderationIndicesRange returns consideration indices involving a given public key
	// over a range of heights. If startHeight > endHeight this iterates in reverse.
	GetPublicKeyConsiderationIndicesRange(
		pubKey ed25519.PublicKey, startHeight, endHeight int64, startIndex, limit int) (
		[]CountID, []int, int64, int, error)

	// Imbalance returns the total current ledger imbalance by summing the imbalance of all public keys.
	// It's only used offline for verification purposes.
	Imbalance() (int64, error)

	// GetPublicKeyImbalanceAt returns the public key imbalance at the given height.
	// It's only used offline for historical and verification purposes.
	// This is only accurate when the full focal point is indexed (pruning disabled.)
	GetPublicKeyImbalanceAt(pubKey ed25519.PublicKey, height int64) (int64, error)
}
