package focalpoint

// ViewStorage is an interface for storing views and their considerations.
type ViewStorage interface {
	// Store is called to store all of the view's information.
	Store(id ViewID, view *View, now int64) error

	// Get returns the referenced view.
	GetView(id ViewID) (*View, error)

	// GetViewBytes returns the referenced view as a byte slice.
	GetViewBytes(id ViewID) ([]byte, error)

	// GetViewHeader returns the referenced view's header and the timestamp of when it was stored.
	GetViewHeader(id ViewID) (*ViewHeader, int64, error)

	// GetConsideration returns a consideration within a view and the view's header.
	GetConsideration(id ViewID, index int) (*Consideration, *ViewHeader, error)
}
