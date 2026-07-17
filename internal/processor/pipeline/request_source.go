package pipeline

import "context"

// RequestSource produces RequestItem values into out.
// Closes out when exhausted or when ctx is cancelled.
// Returns an error for fatal read failures.
type RequestSource interface {
	Produce(ctx context.Context, out chan<- RequestItem) error
}
