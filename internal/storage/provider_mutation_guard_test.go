package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type guardFake struct {
	ids                  []string
	id                   string
	beginErr, resolveErr error
	resolves             int
}

func (f *guardFake) BeginProjectionMutationMany(_ context.Context, ids []string, id string, _ time.Time) error {
	f.ids = ids
	f.id = id
	return f.beginErr
}
func (f *guardFake) ResolveProjectionMutationManyAndSchedule(_ context.Context, ids []string, id string, _ time.Time) error {
	f.resolves++
	if id != f.id {
		return errors.New("wrong id")
	}
	return f.resolveErr
}

type failingEntropy struct{}

func (failingEntropy) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func TestProjectionMutationGuardAdapter(t *testing.T) {
	now := time.Now().UTC()
	f := &guardFake{}
	g := &ProjectionMutationGuard{store: f, entropy: bytes.NewReader(make([]byte, 32)), clock: func() time.Time { return now }}
	ticket, err := g.BeginProviderMutation(t.Context(), []string{"b", "a", "a"})
	if err != nil || ticket == nil || len(f.ids) != 2 || f.ids[0] != "a" || len(f.id) != 64 {
		t.Fatalf("begin: %v", err)
	}
	f.resolveErr = errors.New("reconciliation failed")
	if err = ticket.ResolveProviderMutation(t.Context()); !errors.Is(err, f.resolveErr) {
		t.Fatalf("resolution error lost: %v", err)
	}
	g.entropy = failingEntropy{}
	if _, err = g.BeginProviderMutation(t.Context(), []string{"a"}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("entropy error: %v", err)
	}
	g.entropy = bytes.NewReader(make([]byte, 32))
	f.beginErr = errors.New("uncertain commit")
	ticket, err = g.BeginProviderMutation(t.Context(), []string{"a"})
	var begin *ProjectionMutationBeginError
	if ticket != nil || !errors.As(err, &begin) || !errors.Is(err, f.beginErr) || len(begin.OperationID) != 64 {
		t.Fatalf("ambiguous begin: %v", err)
	}
	if _, err = NewProjectionMutationGuard(nil).BeginProviderMutation(t.Context(), []string{"a"}); err == nil {
		t.Fatal("nil store accepted")
	}
}
