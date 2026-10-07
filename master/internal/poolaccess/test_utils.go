//go:build integration
// +build integration

package poolaccess

import "testing"

// SetReaderForTest makes CanUseResourcePool and UsablePools read restrictions with read until the
// test ends. It returns the reader they used before, so that read can call it. Integration tests
// use it to see the reads that an API request makes, or to make them fail.
func SetReaderForTest(t testing.TB, read RestrictionReader) RestrictionReader {
	previous := defaultChecker
	defaultChecker = NewChecker(read)
	t.Cleanup(func() { defaultChecker = previous })
	return previous.read
}
