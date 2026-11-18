//go:build integration

package storage

import "testing"

func TestGatewayAuthorityPostgresMigration29(t *testing.T) {
	testGatewayAuthority(t, newAuthorityPostgresStore(t, 29))
}

func TestGatewayGracePostgres(t *testing.T) {
	testGatewayGrace(t, newAuthorityPostgresStore(t, 29))
}

func TestGatewayPublicationPostgres(t *testing.T) {
	testGatewayPublication(t, newAuthorityPostgresStore(t, 29))
}

func TestGatewayWriteReceiptPostgres(t *testing.T) {
	testGatewayWriteReceipt(t, newAuthorityPostgresStore(t, 29))
}

func TestGatewayMicrosoftSeriesCoveragePostgres(t *testing.T) {
	testGatewayMicrosoftSeriesCoverage(t, newAuthorityPostgresStore(t, 29))
}

func TestGatewayConfirmationChecksPostgresClock(t *testing.T) {
	testGatewayConfirmationChecksDatabaseClock(t, newAuthorityPostgresStore(t, 29))
}
