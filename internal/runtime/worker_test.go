package runtime

import (
	"context"
	"errors"
	"testing"
)

func TestWorkerRequiresStorage(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	if err := Worker(context.Background()); !errors.Is(err, errWorkerNotConfigured) {
		t.Fatalf("Worker() error = %v, want %v", err, errWorkerNotConfigured)
	}
}
