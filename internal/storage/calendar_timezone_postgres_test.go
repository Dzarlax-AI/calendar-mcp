//go:build integration

package storage

import "testing"

func TestPostgresDiscoveryTimezonePreservation(t *testing.T) {
	runDiscoveryTimezonePreservation(t, newAuthorityPostgresStore(t, SchemaVersion))
}

func TestPostgresAppleTimezoneRefresh(t *testing.T) {
	runAppleTimezoneRefresh(t, newAuthorityPostgresStore(t, SchemaVersion))
}
