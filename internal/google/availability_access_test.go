package google

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestGoogleCapabilityRequiresPositiveWriteRole(t *testing.T) {
	for _, tc := range []struct {
		role  string
		write bool
	}{{"owner", true}, {"writer", true}, {"reader", false}, {"freeBusyReader", false}, {"", false}, {"future-role", false}} {
		t.Run(tc.role, func(t *testing.T) {
			p, closeFn := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "synthetic", "accessRole": tc.role})
			})
			defer closeFn()
			caps, err := p.Capabilities(context.Background(), "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			if caps.Operations.Create != tc.write || caps.Operations.Update != tc.write || caps.Operations.Delete != tc.write || caps.ReadOnly == tc.write {
				t.Fatalf("incorrect write capability role=%q", tc.role)
			}
		})
	}
}
func TestGoogleCatalogCapturesPositiveWriteEvidenceWithoutChangingLegacyReadOnly(t *testing.T) {
	p, closeFn := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"id": "owner", "accessRole": "owner"}, {"id": "reader", "accessRole": "reader"}, {"id": "unknown"}}})
	})
	defer closeFn()
	c, err := p.ListCalendars(context.Background())
	if err != nil || len(c) != 3 {
		t.Fatal("list error")
	}
	if !c[0].GoogleWriteAccessProven || c[1].GoogleWriteAccessProven || c[2].GoogleWriteAccessProven {
		t.Fatal("incorrect positive evidence")
	}
	if c[2].ReadOnly {
		t.Fatal("legacy list behavior unexpectedly changed")
	}
}
