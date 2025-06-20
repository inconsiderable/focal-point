package focalpoint

// CountStorage is an interface for storing counts and their considerations.
type CountStorage interface {
	// Store is called to store all of the count's information.
	Store(id CountID, count *Count, now int64) error

	// Get returns the referenced count.
	GetCount(id CountID) (*Count, error)

	// GetCountBytes returns the referenced count as a byte slice.
	GetCountBytes(id CountID) ([]byte, error)

	// GetCountHeader returns the referenced count's header and the timestamp of when it was stored.
	GetCountHeader(id CountID) (*CountHeader, int64, error)

	// GetConsideration returns a consideration within a count and the count's header.
	GetConsideration(id CountID, index int) (*Consideration, *CountHeader, error)
}
