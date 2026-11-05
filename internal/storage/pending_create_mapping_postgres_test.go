//go:build integration

package storage

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresPendingCreateMappingRace(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	first, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if err = first.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	runPendingCreateMappingRace(t, first, second, fmt.Sprintf("pending-pg-%d", time.Now().UnixNano()))
}
