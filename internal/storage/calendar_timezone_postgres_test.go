//go:build integration

package storage

import (
	"os"
	"testing"
)

func TestPostgresDiscoveryTimezonePreservation(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	s, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	runDiscoveryTimezonePreservation(t, s)
}
