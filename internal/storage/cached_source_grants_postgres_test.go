//go:build integration

package storage

import "testing"

func TestPostgresCachedPresentationChecksGrantAfterDecode(t *testing.T) {
	for _, provider := range []string{"google", "apple", "microsoft"} {
		t.Run(provider, func(t *testing.T) {
			store := newAuthorityPostgresStore(t, SchemaVersion)
			testCachedPresentationReconnect(t, store, store, provider, false)
		})
	}
}
