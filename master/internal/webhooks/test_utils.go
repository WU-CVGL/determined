//go:build integration
// +build integration

package webhooks

import (
	"context"
)

// UseManagerForTests sets the default manager to one read from the database and the shipper to
// one that ships nothing, so that tests of other packages can queue events and read them from
// the queue. It returns a function that restores both.
func UseManagerForTests(ctx context.Context) (restore func(), err error) {
	manager, err := New(ctx)
	if err != nil {
		return nil, err
	}
	oldManager, oldShipper := defaultManager, singletonShipper
	defaultManager = manager
	singletonShipper = &shipper{wake: make(chan<- struct{})}
	return func() {
		defaultManager, singletonShipper = oldManager, oldShipper
	}, nil
}
