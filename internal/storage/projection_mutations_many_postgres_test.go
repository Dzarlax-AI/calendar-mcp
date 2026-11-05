//go:build integration

package storage

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestProjectionMutationManyPostgres(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL is not set")
	}
	ctx := t.Context()
	first, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if err = first.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ensureMutationManySchema(t, first)
	second, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	runProjectionMutationManyContract(t, first, second, fmt.Sprintf("many-pg-%d", time.Now().UnixNano()))
}
