package focalpoint

import (
	"bytes"
	"container/list"
	"encoding/base64"
	"fmt"
	"sync"
)

// ConsiderationQueueMemory is an in-memory FIFO implementation of the ConsiderationQueue interface.
type ConsiderationQueueMemory struct {
	cnMap        	map[ConsiderationID]*list.Element
	cnQueue      	*list.List
	imbalanceCache 	*ImbalanceCache
	conGraph      	*Graph
	lock         	sync.RWMutex
}

// NewConsiderationQueueMemory returns a new NewConsiderationQueueMemory instance.
func NewConsiderationQueueMemory(ledger Ledger, conGraph *Graph) *ConsiderationQueueMemory {

	return &ConsiderationQueueMemory{
		cnMap:        	make(map[ConsiderationID]*list.Element),
		cnQueue:      	list.New(),
		imbalanceCache:	NewImbalanceCache(ledger),
		conGraph: 		conGraph,
	}
}

// Add adds the consideration to the queue. Returns true if the consideration was added to the queue on this call.
func (t *ConsiderationQueueMemory) Add(id ConsiderationID, cn *Consideration) (bool, error) {
	t.lock.Lock()
	defer t.lock.Unlock()
	if _, ok := t.cnMap[id]; ok {
		// already exists
		return false, nil
	}

	// check agent imbalance and update agent and beneficiary imbalances
	ok, err := t.imbalanceCache.Apply(cn)
	if err != nil {
		return false, err
	}
	if !ok {
		// insufficient agent imbalance
		return false, fmt.Errorf("Consideration %s agent %s has no imbalance",
			id, base64.StdEncoding.EncodeToString(cn.By[:]))
	}

	if t.conGraph.IsParentDescendant(pubKeyToString(cn.For), pubKeyToString(cn.By)){
		return false, fmt.Errorf("Agent is a descendant of beneficiary in consideration %s", id)
	}

	// add to the back of the queue
	e := t.cnQueue.PushBack(cn)
	t.cnMap[id] = e
	return true, nil
}

// AddBatch adds a batch of considerations to the queue (a view has been disconnected.)
// "height" is the focal point height after this disconnection.
func (t *ConsiderationQueueMemory) AddBatch(ids []ConsiderationID, cns []*Consideration, height int64) error {
	t.lock.Lock()
	defer t.lock.Unlock()

	// add to front in reverse order.
	// we want formerly confirmed considerations to have the highest
	// priority for getting into the next view.
	for i := len(cns) - 1; i >= 0; i-- {
		if e, ok := t.cnMap[ids[i]]; ok {
			// remove it from its current position
			t.cnQueue.Remove(e)
		}
		e := t.cnQueue.PushFront(cns[i])
		t.cnMap[ids[i]] = e
	}

	// we don't want to invalidate anything based on maturity/expiration/imbalance yet.
	// if we're disconnecting a view we're going to be connecting some shortly.
	return nil
}

// RemoveBatch removes a batch of considerations from the queue (a view has been connected.)
// "height" is the focal point height after this connection.
// "more" indicates if more connections are coming.
func (t *ConsiderationQueueMemory) RemoveBatch(ids []ConsiderationID, height int64, more bool) error {
	t.lock.Lock()
	defer t.lock.Unlock()
	for _, id := range ids {
		e, ok := t.cnMap[id]
		if !ok {
			// not in the queue
			continue
		}
		// remove it
		t.cnQueue.Remove(e)
		delete(t.cnMap, id)
	}

	if more {
		// we don't want to invalidate anything based on series/maturity/expiration/imbalance
		// until we're done connecting all of the views we intend to
		return nil
	}

	return t.reprocessQueue(height)
}

// Rebuild the imbalance cache and remove considerations now in violation
func (t *ConsiderationQueueMemory) reprocessQueue(height int64) error {
	// invalidate the cache
	t.imbalanceCache.Reset()

	// remove invalidated considerations from the queue
	tmpQueue := list.New()
	tmpQueue.PushBackList(t.cnQueue)
	for e := tmpQueue.Front(); e != nil; e = e.Next() {
		cn := e.Value.(*Consideration)
		// check that the series would still be valid
		if !checkConsiderationSeries(cn, height+1) ||
			// check maturity and expiration if included in the next view
			!cn.IsMature(height+1) || cn.IsExpired(height+1) {
			// consideration has been invalidated. remove and continue
			id, err := cn.ID()
			if err != nil {
				return err
			}
			e := t.cnMap[id]
			t.cnQueue.Remove(e)
			delete(t.cnMap, id)
			continue
		}

		// check imbalance
		ok, err := t.imbalanceCache.Apply(cn)
		if err != nil {
			return err
		}
		if !ok || t.conGraph.IsParentDescendant(pubKeyToString(cn.For), pubKeyToString(cn.By)) {
			// consideration has been invalidated. remove and continue
			id, err := cn.ID()
			if err != nil {
				return err
			}
			e := t.cnMap[id]
			t.cnQueue.Remove(e)
			delete(t.cnMap, id)
			continue
		}
	}
	return nil
}

// Get returns considerations in the queue for the renderer.
func (t *ConsiderationQueueMemory) Get(limit int) []*Consideration {
	var cns []*Consideration
	t.lock.RLock()
	defer t.lock.RUnlock()
	if limit == 0 || t.cnQueue.Len() < limit {
		cns = make([]*Consideration, t.cnQueue.Len())
	} else {
		cns = make([]*Consideration, limit)
	}
	i := 0
	for e := t.cnQueue.Front(); e != nil; e = e.Next() {
		cns[i] = e.Value.(*Consideration)
		i++
		if i == limit {
			break
		}
	}
	return cns
}

// Exists returns true if the given consideration is in the queue.
func (t *ConsiderationQueueMemory) Exists(id ConsiderationID) bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	_, ok := t.cnMap[id]
	return ok
}

// ExistsSigned returns true if the given consideration is in the queue and contains the given signature.
func (t *ConsiderationQueueMemory) ExistsSigned(id ConsiderationID, signature Signature) bool {
	t.lock.RLock()
	defer t.lock.RUnlock()
	if e, ok := t.cnMap[id]; ok {
		cn := e.Value.(*Consideration)
		return bytes.Equal(cn.Signature, signature)
	}
	return false
}

// Len returns the queue length.
func (t *ConsiderationQueueMemory) Len() int {
	t.lock.RLock()
	defer t.lock.RUnlock()
	return t.cnQueue.Len()
}
