package focalpoint

import (
	"container/list"
	"sync"
	"time"
)

// CountQueue is a queue of counts to download.
type CountQueue struct {
	countMap   map[CountID]*list.Element
	countQueue *list.List
	lock      sync.RWMutex
}

// If a count has been in the queue for more than 2 minutes it can be re-added with a new peer responsible for its download.
const maxQueueWait = 2 * time.Minute

type countQueueEntry struct {
	id   CountID
	who  string
	when time.Time
}

// NewCountQueue returns a new instance of a CountQueue.
func NewCountQueue() *CountQueue {
	return &CountQueue{
		countMap:   make(map[CountID]*list.Element),
		countQueue: list.New(),
	}
}

// Add adds the count ID to the back of the queue and records the address of the peer who pushed it if it didn't exist in the queue.
// If it did exist and maxQueueWait has elapsed, the count is left in its position but the peer responsible for download is updated.
func (b *CountQueue) Add(id CountID, who string) bool {
	b.lock.Lock()
	defer b.lock.Unlock()
	if e, ok := b.countMap[id]; ok {
		entry := e.Value.(*countQueueEntry)
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
	entry := &countQueueEntry{id: id, who: who, when: time.Now()}
	e := b.countQueue.PushBack(entry)
	b.countMap[id] = e
	return true
}

// Remove removes the count ID from the queue only if the requester is who is currently responsible for its download.
func (b *CountQueue) Remove(id CountID, who string) bool {
	b.lock.Lock()
	defer b.lock.Unlock()
	if e, ok := b.countMap[id]; ok {
		entry := e.Value.(*countQueueEntry)
		if entry.who == who {
			b.countQueue.Remove(e)
			delete(b.countMap, entry.id)
			return true
		}
	}
	return false
}

// Exists returns true if the count ID exists in the queue.
func (b *CountQueue) Exists(id CountID) bool {
	b.lock.RLock()
	defer b.lock.RUnlock()
	_, ok := b.countMap[id]
	return ok
}

// Peek returns the ID of the count at the front of the queue.
func (b *CountQueue) Peek() (CountID, bool) {
	b.lock.RLock()
	defer b.lock.RUnlock()
	if b.countQueue.Len() == 0 {
		return CountID{}, false
	}
	e := b.countQueue.Front()
	entry := e.Value.(*countQueueEntry)
	return entry.id, true
}

// Len returns the length of the queue.
func (b *CountQueue) Len() int {
	b.lock.RLock()
	defer b.lock.RUnlock()
	return b.countQueue.Len()
}
