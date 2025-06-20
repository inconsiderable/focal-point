package focalpoint

import (
	"container/list"
	"sync"
	"time"
)

// ViewQueue is a queue of views to download.
type ViewQueue struct {
	viewMap   map[ViewID]*list.Element
	viewQueue *list.List
	lock       sync.RWMutex
}

// If a view has been in the queue for more than 2 minutes it can be re-added with a new peer responsible for its download.
const maxQueueWait = 2 * time.Minute

type viewQueueEntry struct {
	id   ViewID
	who  string
	when time.Time
}

// NewViewQueue returns a new instance of a ViewQueue.
func NewViewQueue() *ViewQueue {
	return &ViewQueue{
		viewMap:   make(map[ViewID]*list.Element),
		viewQueue: list.New(),
	}
}

// Add adds the view ID to the back of the queue and records the address of the peer who pushed it if it didn't exist in the queue.
// If it did exist and maxQueueWait has elapsed, the view is left in its position but the peer responsible for download is updated.
func (b *ViewQueue) Add(id ViewID, who string) bool {
	b.lock.Lock()
	defer b.lock.Unlock()
	if e, ok := b.viewMap[id]; ok {
		entry := e.Value.(*viewQueueEntry)
		if time.Since(entry.when) < maxQueueWait {
			// it's still pending download
			return false
		}
		// it's expired. signal that it can be tried again and leave it in place
		entry.when = time.Now()
		// new peer owns its place in the queue
		entry.who = who
		return true
	}

	// add to the back of the queue
	entry := &viewQueueEntry{id: id, who: who, when: time.Now()}
	e := b.viewQueue.PushBack(entry)
	b.viewMap[id] = e
	return true
}

// Remove removes the view ID from the queue only if the requester is who is currently responsible for its download.
func (b *ViewQueue) Remove(id ViewID, who string) bool {
	b.lock.Lock()
	defer b.lock.Unlock()
	if e, ok := b.viewMap[id]; ok {
		entry := e.Value.(*viewQueueEntry)
		if entry.who == who {
			b.viewQueue.Remove(e)
			delete(b.viewMap, entry.id)
			return true
		}
	}
	return false
}

// Exists returns true if the view ID exists in the queue.
func (b *ViewQueue) Exists(id ViewID) bool {
	b.lock.RLock()
	defer b.lock.RUnlock()
	_, ok := b.viewMap[id]
	return ok
}

// Peek returns the ID of the view at the front of the queue.
func (b *ViewQueue) Peek() (ViewID, bool) {
	b.lock.RLock()
	defer b.lock.RUnlock()
	if b.viewQueue.Len() == 0 {
		return ViewID{}, false
	}
	e := b.viewQueue.Front()
	entry := e.Value.(*viewQueueEntry)
	return entry.id, true
}

// Len returns the length of the queue.
func (b *ViewQueue) Len() int {
	b.lock.RLock()
	defer b.lock.RUnlock()
	return b.viewQueue.Len()
}
