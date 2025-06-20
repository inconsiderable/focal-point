package focalpoint

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"golang.org/x/crypto/ed25519"
)

// LedgerDisk is an on-disk implemenation of the Ledger interface using LevelDB.
type LedgerDisk struct {
	db         	*leveldb.DB
	viewStore 	ViewStorage
	conGraph 	*Graph
	prune      	bool // prune historic consideration and public key consideration indices
}

// NewLedgerDisk returns a new instance of LedgerDisk.
func NewLedgerDisk(dbPath string, readOnly, prune bool, viewStore ViewStorage, conGraph *Graph) (*LedgerDisk, error) {
	opts := opt.Options{ReadOnly: readOnly}
	db, err := leveldb.OpenFile(dbPath, &opts)
	if err != nil {
		return nil, err
	}
	return &LedgerDisk{db: db, viewStore: viewStore, conGraph: *&conGraph, prune: prune}, nil
}

// GetPointTip returns the ID and the height of the view at the current tip of the main point.
func (l LedgerDisk) GetPointTip() (*ViewID, int64, error) {
	return getPointTip(l.db)
}

// Sometimes we call this with *leveldb.DB or *leveldb.Snapshot
func getPointTip(db leveldb.Reader) (*ViewID, int64, error) {
	// compute db key
	key, err := computePointTipKey()
	if err != nil {
		return nil, 0, err
	}

	// fetch the id
	ctBytes, err := db.Get(key, nil)
	if err == leveldb.ErrNotFound {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}

	// decode the tip
	id, height, err := decodePointTip(ctBytes)
	if err != nil {
		return nil, 0, err
	}

	return id, height, nil
}

// GetViewIDForHeight returns the ID of the view at the given focal point height.
func (l LedgerDisk) GetViewIDForHeight(height int64) (*ViewID, error) {
	return getViewIDForHeight(height, l.db)
}

// Sometimes we call this with *leveldb.DB or *leveldb.Snapshot
func getViewIDForHeight(height int64, db leveldb.Reader) (*ViewID, error) {
	// compute db key
	key, err := computeViewHeightIndexKey(height)
	if err != nil {
		return nil, err
	}

	// fetch the id
	idBytes, err := db.Get(key, nil)
	if err == leveldb.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// return it
	id := new(ViewID)
	copy(id[:], idBytes)
	return id, nil
}

// SetBranchType sets the branch type for the given view.
func (l LedgerDisk) SetBranchType(id ViewID, branchType BranchType) error {
	// compute db key
	key, err := computeBranchTypeKey(id)
	if err != nil {
		return err
	}

	// write type
	wo := opt.WriteOptions{Sync: true}
	return l.db.Put(key, []byte{byte(branchType)}, &wo)
}

// GetBranchType returns the branch type for the given view.
func (l LedgerDisk) GetBranchType(id ViewID) (BranchType, error) {
	// compute db key
	key, err := computeBranchTypeKey(id)
	if err != nil {
		return UNKNOWN, err
	}

	// fetch type
	branchType, err := l.db.Get(key, nil)
	if err == leveldb.ErrNotFound {
		return UNKNOWN, nil
	}
	if err != nil {
		return UNKNOWN, err
	}
	return BranchType(branchType[0]), nil
}

// ConnectView connects a view to the tip of the focal point and applies the considerations to the ledger.
func (l LedgerDisk) ConnectView(id ViewID, view *View) ([]ConsiderationID, error) {
	// sanity check
	tipID, _, err := l.GetPointTip()
	if err != nil {
		return nil, err
	}
	if tipID != nil && *tipID != view.Header.Previous {
		return nil, fmt.Errorf("Being asked to connect %s but previous %s does not match tip %s",
			id, view.Header.Previous, *tipID)
	}

	// apply all resulting writes atomically
	batch := new(leveldb.Batch)

	imbalanceCache := NewImbalanceCache(l)
	cnIDs := make([]ConsiderationID, len(view.Considerations))

	for i, cn := range view.Considerations {
		cnID, err := cn.ID()
		if err != nil {
			return nil, err
		}
		cnIDs[i] = cnID

		// verify the consideration hasn't been processed already.
		// note that we can safely prune indices for considerations older than the previous series
		key, err := computeConsiderationIndexKey(cnID)
		if err != nil {
			return nil, err
		}
		ok, err := l.db.Has(key, nil)
		if err != nil {
			return nil, err
		}
		if ok {
			return nil, fmt.Errorf("Consideration %s already processed", cnID)
		}

		// set the consideration index now
		indexBytes, err := encodeConsiderationIndex(view.Header.Height, i)
		if err != nil {
			return nil, err
		}
		batch.Put(key, indexBytes)

		cnToApply := cn

		if cn.IsViewpoint() {
			// don't apply a viewpoint to a imbalance until it's x views deep.
			// during honest reorgs normal considerations usually get into the new most-work branch
			// but viewpoints vanish. this mitigates the impact on UX when reorgs occur and considerations
			// depend on viewpoints.
			cnToApply = nil

			if view.Header.Height-VIEWPOINT_MATURITY >= 0 {
				// mature the viewpoint from 100 views ago now
				oldID, err := l.GetViewIDForHeight(view.Header.Height - VIEWPOINT_MATURITY)
				if err != nil {
					return nil, err
				}
				if oldID == nil {
					return nil, fmt.Errorf("Missing view at height %d\n",
						view.Header.Height-VIEWPOINT_MATURITY)
				}

				// we could store the last 100 viewpoints on our own in memory if we end up needing to
				oldTx, _, err := l.viewStore.GetConsideration(*oldID, 0)
				if err != nil {
					return nil, err
				}
				if oldTx == nil {
					return nil, fmt.Errorf("Missing viewpoint from view %s\n", *oldID)
				}

				// apply it to the recipient's imbalance
				cnToApply = oldTx
			}
		}

		if cnToApply != nil {
			// check sender imbalance and update sender and receiver imbalances
			ok, err := imbalanceCache.Apply(cnToApply)
			if err != nil {
				return nil, err
			}
			if !ok {
				cnID, _ := cnToApply.ID()
				return nil, fmt.Errorf("Sender has insufficient imbalance in consideration %s", cnID)
			}

			if l.conGraph.IsParentDescendant(pubKeyToString(cnToApply.For), pubKeyToString(cnToApply.By)){
				cnID, _ := cnToApply.ID()
				return nil, fmt.Errorf("Sender is a descendant of recipient in consideration %s", cnID)
			}
		}

		// associate this consideration with both parties
		if !cn.IsViewpoint() {
			key, err = computePubKeyConsiderationIndexKey(cn.By, &view.Header.Height, &i)
			if err != nil {
				return nil, err
			}
			batch.Put(key, []byte{0x1})
		}
		key, err = computePubKeyConsiderationIndexKey(cn.For, &view.Header.Height, &i)
		if err != nil {
			return nil, err
		}
		batch.Put(key, []byte{0x1})
	}

	// update recorded imbalances
	imbalances := imbalanceCache.Imbalances()
	for pubKeyBytes, imbalance := range imbalances {
		key, err := computePubKeyImbalanceKey(ed25519.PublicKey(pubKeyBytes[:]))
		if err != nil {
			return nil, err
		}
		if imbalance == 0 {
			batch.Delete(key)
		} else {
			imbalanceBytes, err := encodeNumber(imbalance)
			if err != nil {
				return nil, err
			}
			batch.Put(key, imbalanceBytes)
		}
	}

	// index the view by height
	key, err := computeViewHeightIndexKey(view.Header.Height)
	if err != nil {
		return nil, err
	}
	batch.Put(key, id[:])

	// set this view on the main point
	key, err = computeBranchTypeKey(id)
	if err != nil {
		return nil, err
	}
	batch.Put(key, []byte{byte(MAIN)})

	// set this view as the new tip
	key, err = computePointTipKey()
	if err != nil {
		return nil, err
	}
	ctBytes, err := encodePointTip(id, view.Header.Height)
	if err != nil {
		return nil, err
	}
	batch.Put(key, ctBytes)

	// prune historic consideration and public key consideration indices now
	if l.prune && view.Header.Height >= 2*VIEWS_UNTIL_NEW_SERIES {
		if err := l.pruneIndices(view.Header.Height-2*VIEWS_UNTIL_NEW_SERIES, batch); err != nil {
			return nil, err
		}
	}

	// perform the writes
	wo := opt.WriteOptions{Sync: true}
	if err := l.db.Write(batch, &wo); err != nil {
		return nil, err
	}

	return cnIDs, nil
}

// DisconnectView disconnects a view from the tip of the focal point and undoes the effects of the considerations on the ledger.
func (l LedgerDisk) DisconnectView(id ViewID, view *View) ([]ConsiderationID, error) {
	// sanity check
	tipID, _, err := l.GetPointTip()
	if err != nil {
		return nil, err
	}
	if tipID == nil {
		return nil, fmt.Errorf("Being asked to disconnect %s but no tip is currently set",
			id)
	}
	if *tipID != id {
		return nil, fmt.Errorf("Being asked to disconnect %s but it does not match tip %s",
			id, *tipID)
	}

	// apply all resulting writes atomically
	batch := new(leveldb.Batch)

	imbalanceCache := NewImbalanceCache(l)
	cnIDs := make([]ConsiderationID, len(view.Considerations))

	// disconnect considerations in reverse order
	for i := len(view.Considerations) - 1; i >= 0; i-- {
		cn := view.Considerations[i]
		cnID, err := cn.ID()
		if err != nil {
			return nil, err
		}
		// save the id
		cnIDs[i] = cnID

		// mark the consideration unprocessed now (delete its index)
		key, err := computeConsiderationIndexKey(cnID)
		if err != nil {
			return nil, err
		}
		batch.Delete(key)

		cnToUndo := cn
		if cn.IsViewpoint() {
			// viewpoint doesn't affect recipient imbalance for x more views
			cnToUndo = nil

			if view.Header.Height-VIEWPOINT_MATURITY >= 0 {
				// undo the effect of the viewpoint from x views ago now
				oldID, err := l.GetViewIDForHeight(view.Header.Height - VIEWPOINT_MATURITY)
				if err != nil {
					return nil, err
				}
				if oldID == nil {
					return nil, fmt.Errorf("Missing view at height %d\n",
						view.Header.Height-VIEWPOINT_MATURITY)
				}
				oldTx, _, err := l.viewStore.GetConsideration(*oldID, 0)
				if err != nil {
					return nil, err
				}
				if oldTx == nil {
					return nil, fmt.Errorf("Missing viewpoint from view %s\n", *oldID)
				}

				// undo its effect on the recipient's imbalance
				cnToUndo = oldTx
			}
		}

		if cnToUndo != nil {
			// credit sender and debit recipient
			err = imbalanceCache.Undo(cnToUndo)
			if err != nil {
				return nil, err
			}
		}

		// unassociate this consideration with both parties
		if !cn.IsViewpoint() {
			key, err = computePubKeyConsiderationIndexKey(cn.By, &view.Header.Height, &i)
			if err != nil {
				return nil, err
			}
			batch.Delete(key)
		}
		key, err = computePubKeyConsiderationIndexKey(cn.For, &view.Header.Height, &i)
		if err != nil {
			return nil, err
		}
		batch.Delete(key)
	}

	// update recorded imbalances
	imbalances := imbalanceCache.Imbalances()
	for pubKeyBytes, imbalance := range imbalances {
		key, err := computePubKeyImbalanceKey(ed25519.PublicKey(pubKeyBytes[:]))
		if err != nil {
			return nil, err
		}
		if imbalance == 0 {
			batch.Delete(key)
		} else {
			imbalanceBytes, err := encodeNumber(imbalance)
			if err != nil {
				return nil, err
			}
			batch.Put(key, imbalanceBytes)
		}
	}

	// remove this view's index by height
	key, err := computeViewHeightIndexKey(view.Header.Height)
	if err != nil {
		return nil, err
	}
	batch.Delete(key)

	// set this view on a side point
	key, err = computeBranchTypeKey(id)
	if err != nil {
		return nil, err
	}
	batch.Put(key, []byte{byte(SIDE)})

	// set the previous view as the point tip
	key, err = computePointTipKey()
	if err != nil {
		return nil, err
	}
	ctBytes, err := encodePointTip(view.Header.Previous, view.Header.Height-1)
	if err != nil {
		return nil, err
	}
	batch.Put(key, ctBytes)

	// restore historic indices now
	if l.prune && view.Header.Height >= 2*VIEWS_UNTIL_NEW_SERIES {
		if err := l.restoreIndices(view.Header.Height-2*VIEWS_UNTIL_NEW_SERIES, batch); err != nil {
			return nil, err
		}
	}

	// perform the writes
	wo := opt.WriteOptions{Sync: true}
	if err := l.db.Write(batch, &wo); err != nil {
		return nil, err
	}

	return cnIDs, nil
}

// Prune consideration and public key consideration indices created by the view at the given height
func (l LedgerDisk) pruneIndices(height int64, batch *leveldb.Batch) error {
	// get the ID
	id, err := l.GetViewIDForHeight(height)
	if err != nil {
		return err
	}
	if id == nil {
		return fmt.Errorf("Missing view ID for height %d\n", height)
	}

	// fetch the view
	view, err := l.viewStore.GetView(*id)
	if err != nil {
		return err
	}
	if view == nil {
		return fmt.Errorf("Missing view %s\n", *id)
	}

	for i, cn := range view.Considerations {
		cnID, err := cn.ID()
		if err != nil {
			return err
		}

		// prune consideration index
		key, err := computeConsiderationIndexKey(cnID)
		if err != nil {
			return err
		}
		batch.Delete(key)

		// prune public key consideration indices
		if !cn.IsViewpoint() {
			key, err = computePubKeyConsiderationIndexKey(cn.By, &view.Header.Height, &i)
			if err != nil {
				return err
			}
			batch.Delete(key)
		}
		key, err = computePubKeyConsiderationIndexKey(cn.For, &view.Header.Height, &i)
		if err != nil {
			return err
		}
		batch.Delete(key)
	}

	return nil
}

// Restore consideration and public key consideration indices created by the view at the given height
func (l LedgerDisk) restoreIndices(height int64, batch *leveldb.Batch) error {
	// get the ID
	id, err := l.GetViewIDForHeight(height)
	if err != nil {
		return err
	}
	if id == nil {
		return fmt.Errorf("Missing view ID for height %d\n", height)
	}

	// fetch the view
	view, err := l.viewStore.GetView(*id)
	if err != nil {
		return err
	}
	if view == nil {
		return fmt.Errorf("Missing view %s\n", *id)
	}

	for i, cn := range view.Considerations {
		cnID, err := cn.ID()
		if err != nil {
			return err
		}

		// restore consideration index
		key, err := computeConsiderationIndexKey(cnID)
		if err != nil {
			return err
		}
		indexBytes, err := encodeConsiderationIndex(view.Header.Height, i)
		if err != nil {
			return err
		}
		batch.Put(key, indexBytes)

		// restore public key consideration indices
		if !cn.IsViewpoint() {
			key, err = computePubKeyConsiderationIndexKey(cn.By, &view.Header.Height, &i)
			if err != nil {
				return err
			}
			batch.Put(key, []byte{0x1})
		}
		key, err = computePubKeyConsiderationIndexKey(cn.For, &view.Header.Height, &i)
		if err != nil {
			return err
		}
		batch.Put(key, []byte{0x1})
	}

	return nil
}

// GetPublicKeyImbalance returns the current imbalance of a given public key.
func (l LedgerDisk) GetPublicKeyImbalance(pubKey ed25519.PublicKey) (int64, error) {
	// compute db key
	key, err := computePubKeyImbalanceKey(pubKey)
	if err != nil {
		return 0, err
	}

	// fetch imbalance
	imbalanceBytes, err := l.db.Get(key, nil)
	if err == leveldb.ErrNotFound {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	// decode and return it
	var imbalance int64
	buf := bytes.NewReader(imbalanceBytes)
	binary.Read(buf, binary.BigEndian, &imbalance)
	return imbalance, nil
}

// GetPublicKeyImbalances returns the current imbalance of the given public keys
// along with view ID and height of the corresponding main point tip.
func (l LedgerDisk) GetPublicKeyImbalances(pubKeys []ed25519.PublicKey) (
	map[[ed25519.PublicKeySize]byte]int64, *ViewID, int64, error) {

	// get a consistent view across all queries
	snapshot, err := l.db.GetSnapshot()
	if err != nil {
		return nil, nil, 0, err
	}
	defer snapshot.Release()

	// get the point tip
	tipID, tipHeight, err := getPointTip(snapshot)
	if err != nil {
		return nil, nil, 0, err
	}

	imbalances := make(map[[ed25519.PublicKeySize]byte]int64)

	for _, pubKey := range pubKeys {
		// compute imbalance db key
		key, err := computePubKeyImbalanceKey(pubKey)
		if err != nil {
			return nil, nil, 0, err
		}

		var pk [ed25519.PublicKeySize]byte
		copy(pk[:], pubKey)

		// fetch imbalance
		imbalanceBytes, err := snapshot.Get(key, nil)
		if err == leveldb.ErrNotFound {
			imbalances[pk] = 0
			continue
		}
		if err != nil {
			return nil, nil, 0, err
		}

		// decode it
		var imbalance int64
		buf := bytes.NewReader(imbalanceBytes)
		binary.Read(buf, binary.BigEndian, &imbalance)

		// save it
		imbalances[pk] = imbalance
	}

	return imbalances, tipID, tipHeight, nil
}

// GetConsiderationIndex returns the index of a processed consideration.
func (l LedgerDisk) GetConsiderationIndex(id ConsiderationID) (*ViewID, int, error) {
	// compute the db key
	key, err := computeConsiderationIndexKey(id)
	if err != nil {
		return nil, 0, err
	}

	// we want a consistent view during our two queries as height can change
	snapshot, err := l.db.GetSnapshot()
	if err != nil {
		return nil, 0, err
	}
	defer snapshot.Release()

	// fetch and decode the index
	indexBytes, err := snapshot.Get(key, nil)
	if err == leveldb.ErrNotFound {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	height, index, err := decodeConsiderationIndex(indexBytes)
	if err != nil {
		return nil, 0, err
	}

	// map height to view id
	viewID, err := getViewIDForHeight(height, snapshot)
	if err != nil {
		return nil, 0, err
	}

	// return it
	return viewID, index, nil
}

// GetPublicKeyConsiderationIndicesRange returns consideration indices involving a given public key
// over a range of heights. If startHeight > endHeight this iterates in reverse.
func (l LedgerDisk) GetPublicKeyConsiderationIndicesRange(
	pubKey ed25519.PublicKey, startHeight, endHeight int64, startIndex, limit int) (
	[]ViewID, []int, int64, int, error) {

	if endHeight >= startHeight {
		// forward
		return l.getPublicKeyConsiderationIndicesRangeForward(
			pubKey, startHeight, endHeight, startIndex, limit)
	}

	// reverse
	return l.getPublicKeyConsiderationIndicesRangeReverse(
		pubKey, startHeight, endHeight, startIndex, limit)
}

// Iterate through consideration history going forward
func (l LedgerDisk) getPublicKeyConsiderationIndicesRangeForward(
	pubKey ed25519.PublicKey, startHeight, endHeight int64, startIndex, limit int) (
	ids []ViewID, indices []int, lastHeight int64, lastIndex int, err error) {
	startKey, err := computePubKeyConsiderationIndexKey(pubKey, &startHeight, &startIndex)
	if err != nil {
		return
	}

	endHeight += 1 // make it inclusive
	endKey, err := computePubKeyConsiderationIndexKey(pubKey, &endHeight, nil)
	if err != nil {
		return
	}

	heightMap := make(map[int64]*ViewID)

	// we want a consistent view of this. heights can change out from under us otherwise
	snapshot, err := l.db.GetSnapshot()
	if err != nil {
		return
	}
	defer snapshot.Release()

	iter := snapshot.NewIterator(&util.Range{Start: startKey, Limit: endKey}, nil)
	for iter.Next() {
		_, lastHeight, lastIndex, err = decodePubKeyConsiderationIndexKey(iter.Key())
		if err != nil {
			iter.Release()
			return nil, nil, 0, 0, err
		}

		// lookup the view id
		id, ok := heightMap[lastHeight]
		if !ok {
			var err error
			id, err = getViewIDForHeight(lastHeight, snapshot)
			if err != nil {
				iter.Release()
				return nil, nil, 0, 0, err
			}
			if id == nil {
				iter.Release()
				return nil, nil, 0, 0, fmt.Errorf(
					"No view found at height %d", lastHeight)
			}
			heightMap[lastHeight] = id
		}

		ids = append(ids, *id)
		indices = append(indices, lastIndex)
		if limit != 0 && len(indices) == limit {
			break
		}
	}
	iter.Release()
	if err := iter.Error(); err != nil {
		return nil, nil, 0, 0, err
	}
	return
}

// Iterate through consideration history in reverse
func (l LedgerDisk) getPublicKeyConsiderationIndicesRangeReverse(
	pubKey ed25519.PublicKey, startHeight, endHeight int64, startIndex, limit int) (
	ids []ViewID, indices []int, lastHeight int64, lastIndex int, err error) {
	endKey, err := computePubKeyConsiderationIndexKey(pubKey, &endHeight, nil)
	if err != nil {
		return
	}

	// make it inclusive
	startIndex += 1
	startKey, err := computePubKeyConsiderationIndexKey(pubKey, &startHeight, &startIndex)
	if err != nil {
		return
	}

	heightMap := make(map[int64]*ViewID)

	// we want a consistent view of this. heights can change out from under us otherwise
	snapshot, err := l.db.GetSnapshot()
	if err != nil {
		return
	}
	defer snapshot.Release()

	iter := snapshot.NewIterator(&util.Range{Start: endKey, Limit: startKey}, nil)
	for ok := iter.Last(); ok; ok = iter.Prev() {
		_, lastHeight, lastIndex, err = decodePubKeyConsiderationIndexKey(iter.Key())
		if err != nil {
			iter.Release()
			return nil, nil, 0, 0, err
		}

		// lookup the view id
		id, ok := heightMap[lastHeight]
		if !ok {
			var err error
			id, err = getViewIDForHeight(lastHeight, snapshot)
			if err != nil {
				iter.Release()
				return nil, nil, 0, 0, err
			}
			if id == nil {
				iter.Release()
				return nil, nil, 0, 0, fmt.Errorf(
					"No view found at height %d", lastHeight)
			}
			heightMap[lastHeight] = id
		}

		ids = append(ids, *id)
		indices = append(indices, lastIndex)
		if limit != 0 && len(indices) == limit {
			break
		}
	}
	iter.Release()
	if err := iter.Error(); err != nil {
		return nil, nil, 0, 0, err
	}
	return
}

// Imbalance returns the total current ledger imbalance by summing the imbalance of all public keys.
// It's only used offline for verification purposes.
func (l LedgerDisk) Imbalance() (int64, error) {
	var total int64

	// compute the sum of all public key imbalances
	key, err := computePubKeyImbalanceKey(nil)
	if err != nil {
		return 0, err
	}
	iter := l.db.NewIterator(util.BytesPrefix(key), nil)
	for iter.Next() {
		var imbalance int64
		buf := bytes.NewReader(iter.Value())
		binary.Read(buf, binary.BigEndian, &imbalance)
		total += imbalance
	}
	iter.Release()
	if err := iter.Error(); err != nil {
		return 0, err
	}

	return total, nil
}

// GetPublicKeyImbalanceAt returns the public key imbalance at the given height.
// It's only used offline for historical and verification purposes.
// This is only accurate when the full focal point is indexed (pruning disabled.)
func (l LedgerDisk) GetPublicKeyImbalanceAt(pubKey ed25519.PublicKey, height int64) (int64, error) {
	_, currentHeight, err := l.GetPointTip()
	if err != nil {
		return 0, err
	}

	startKey, err := computePubKeyConsiderationIndexKey(pubKey, nil, nil)
	if err != nil {
		return 0, err
	}

	height += 1 // make it inclusive
	endKey, err := computePubKeyConsiderationIndexKey(pubKey, &height, nil)
	if err != nil {
		return 0, err
	}

	var imbalance int64
	iter := l.db.NewIterator(&util.Range{Start: startKey, Limit: endKey}, nil)
	for iter.Next() {
		_, height, index, err := decodePubKeyConsiderationIndexKey(iter.Key())
		if err != nil {
			iter.Release()
			return 0, err
		}

		if index == 0 && height > currentHeight-VIEWPOINT_MATURITY {
			// viewpoint isn't mature
			continue
		}

		id, err := l.GetViewIDForHeight(height)
		if err != nil {
			iter.Release()
			return 0, err
		}
		if id == nil {
			iter.Release()
			return 0, fmt.Errorf("No view found at height %d", height)
		}

		cn, _, err := l.viewStore.GetConsideration(*id, index)
		if err != nil {
			iter.Release()
			return 0, err
		}
		if cn == nil {
			iter.Release()
			return 0, fmt.Errorf("No consideration found in view %s at index %d",
				*id, index)
		}

		if bytes.Equal(pubKey, cn.For) {
			imbalance += 1
		} else if bytes.Equal(pubKey, cn.By) {
			imbalance -= 1
		} else {
			iter.Release()
			cnID, _ := cn.ID()
			return 0, fmt.Errorf("Consideration %s doesn't involve the public key", cnID)
		}
	}
	iter.Release()
	if err := iter.Error(); err != nil {
		return 0, err
	}
	return imbalance, nil
}

// Close is called to close any underlying storage.
func (l LedgerDisk) Close() error {
	return l.db.Close()
}

// leveldb schema

// T                    -> {bid}{height} (main point tip)
// B{bid}               -> main|side|orphan (1 byte)
// h{height}            -> {bid}
// t{cnid}              -> {height}{index} (prunable up to the previous series)
// k{pk}{height}{index} -> 1 (not strictly necessary. probably should make it optional by flag)
// b{pk}                -> {imbalance} (we always need all of this table)

const pointTipPrefix = 'T'

const branchTypePrefix = 'B'

const viewHeightIndexPrefix = 'h'

const considerationIndexPrefix = 't'

const pubKeyConsiderationIndexPrefix = 'k'

const pubKeyImbalancePrefix = 'b'

func computeBranchTypeKey(id ViewID) ([]byte, error) {
	key := new(bytes.Buffer)
	if err := key.WriteByte(branchTypePrefix); err != nil {
		return nil, err
	}
	if err := binary.Write(key, binary.BigEndian, id[:]); err != nil {
		return nil, err
	}
	return key.Bytes(), nil
}

func computeViewHeightIndexKey(height int64) ([]byte, error) {
	key := new(bytes.Buffer)
	if err := key.WriteByte(viewHeightIndexPrefix); err != nil {
		return nil, err
	}
	if err := binary.Write(key, binary.BigEndian, height); err != nil {
		return nil, err
	}
	return key.Bytes(), nil
}

func computePointTipKey() ([]byte, error) {
	key := new(bytes.Buffer)
	if err := key.WriteByte(pointTipPrefix); err != nil {
		return nil, err
	}
	return key.Bytes(), nil
}

func computeConsiderationIndexKey(id ConsiderationID) ([]byte, error) {
	key := new(bytes.Buffer)
	if err := key.WriteByte(considerationIndexPrefix); err != nil {
		return nil, err
	}
	if err := binary.Write(key, binary.BigEndian, id[:]); err != nil {
		return nil, err
	}
	return key.Bytes(), nil
}

func computePubKeyConsiderationIndexKey(
	pubKey ed25519.PublicKey, height *int64, index *int) ([]byte, error) {
	key := new(bytes.Buffer)
	if err := key.WriteByte(pubKeyConsiderationIndexPrefix); err != nil {
		return nil, err
	}
	if err := binary.Write(key, binary.BigEndian, pubKey); err != nil {
		return nil, err
	}
	if height == nil {
		return key.Bytes(), nil
	}
	if err := binary.Write(key, binary.BigEndian, *height); err != nil {
		return nil, err
	}
	if index == nil {
		return key.Bytes(), nil
	}
	index32 := int32(*index)
	if err := binary.Write(key, binary.BigEndian, index32); err != nil {
		return nil, err
	}
	return key.Bytes(), nil
}

func decodePubKeyConsiderationIndexKey(key []byte) (ed25519.PublicKey, int64, int, error) {
	buf := bytes.NewBuffer(key)
	if _, err := buf.ReadByte(); err != nil {
		return nil, 0, 0, err
	}
	var pubKey [ed25519.PublicKeySize]byte
	if err := binary.Read(buf, binary.BigEndian, pubKey[:32]); err != nil {
		return nil, 0, 0, err
	}
	var height int64
	if err := binary.Read(buf, binary.BigEndian, &height); err != nil {
		return nil, 0, 0, err
	}
	var index int32
	if err := binary.Read(buf, binary.BigEndian, &index); err != nil {
		return nil, 0, 0, err
	}
	return ed25519.PublicKey(pubKey[:]), height, int(index), nil
}

func computePubKeyImbalanceKey(pubKey ed25519.PublicKey) ([]byte, error) {
	key := new(bytes.Buffer)
	if err := key.WriteByte(pubKeyImbalancePrefix); err != nil {
		return nil, err
	}
	if err := binary.Write(key, binary.BigEndian, pubKey); err != nil {
		return nil, err
	}
	return key.Bytes(), nil
}

func encodePointTip(id ViewID, height int64) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.BigEndian, id); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.BigEndian, height); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodePointTip(ctBytes []byte) (*ViewID, int64, error) {
	buf := bytes.NewBuffer(ctBytes)
	id := new(ViewID)
	if err := binary.Read(buf, binary.BigEndian, id); err != nil {
		return nil, 0, err
	}
	var height int64
	if err := binary.Read(buf, binary.BigEndian, &height); err != nil {
		return nil, 0, err
	}
	return id, height, nil
}

func encodeNumber(num int64) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.BigEndian, num); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeConsiderationIndex(height int64, index int) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.BigEndian, height); err != nil {
		return nil, err
	}
	index32 := int32(index)
	if err := binary.Write(buf, binary.BigEndian, index32); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeConsiderationIndex(indexBytes []byte) (int64, int, error) {
	buf := bytes.NewBuffer(indexBytes)
	var height int64
	if err := binary.Read(buf, binary.BigEndian, &height); err != nil {
		return 0, 0, err
	}
	var index int32
	if err := binary.Read(buf, binary.BigEndian, &index); err != nil {
		return 0, 0, err
	}
	return height, int(index), nil
}
