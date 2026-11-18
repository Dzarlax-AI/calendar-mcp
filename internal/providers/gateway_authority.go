package providers

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

func (f *Factory) CheckGatewayAuthority(ctx context.Context, lease storage.GatewayAuthorityLease) ([]storage.GatewayCalendarProof, error) {
	snapshot, err := f.store.CredentialSnapshot(ctx, lease.ConnectionID)
	if err != nil {
		return nil, err
	}
	if snapshot.Provider != lease.Provider || snapshot.Status != "connected" || snapshot.AuthorizationRevision != lease.AuthorizationRevision {
		return nil, storage.ErrAuthorityChanged
	}
	calendars, err := f.store.ListCalendars(ctx, lease.ConnectionID)
	if err != nil {
		return nil, err
	}
	receipts := make(map[string]string, len(calendars))
	for _, item := range calendars {
		receipts[item.ID] = item.DetailReceipt
	}
	var provider calendar.Provider
	if lease.Provider == "google" {
		// This check publishes through its durable lease. Legacy observers
		// close authority directly and must never run on this provider.
		provider, err = f.buildGoogleVerification(snapshot)
	} else {
		provider, err = f.BuildNonGoogleVerification(snapshot)
	}
	if err != nil {
		return nil, err
	}
	checker, ok := provider.(calendar.CalendarReadEvidenceProvider)
	if !ok {
		return nil, errors.New("provider has no gateway calendar authority checker")
	}
	ctx = calendar.WithGatewayReadRestriction(ctx, func(callCtx context.Context, rawID string, eventsDenied bool) error {
		id := calendar.AccountCalendarID(lease.Provider, lease.ConnectionID, rawID)
		return f.store.RestrictGatewayCalendarRead(callCtx, lease, rawID, receipts[id], eventsDenied, time.Now())
	})
	items, err := checker.CheckCalendarReadEvidence(ctx)
	if err != nil {
		var api *calendar.APIError
		var syncErr *calendar.EventSyncError
		if errors.As(err, &api) && api.Code == calendar.ErrorPermissionDenied ||
			errors.As(err, &syncErr) && syncErr.Class == calendar.EventSyncAuth {
			return nil, &storage.GatewayAuthorityDenial{CredentialDigest: sha256.Sum256(snapshot.EncryptedCredentials)}
		}
		return nil, err
	}
	result := make([]storage.GatewayCalendarProof, 0, len(items))
	for _, item := range items {
		id := calendar.AccountCalendarID(lease.Provider, lease.ConnectionID, item.Calendar.ID)
		item.Calendar.ID, item.Calendar.Provider = id, lease.Provider
		item.Capabilities.CalendarID, item.Capabilities.Provider = id, lease.Provider
		class := storage.GatewayMetadataOnly
		if item.ReadEvents {
			class = storage.GatewayEventAccess
			if item.ReadPrivate {
				class = storage.GatewayPrivateAccess
			}
		}
		result = append(result, storage.GatewayCalendarProof{
			Metadata: item.Calendar, Capabilities: item.Capabilities,
			Class: class, Unknown: item.Unknown, GoogleRole: item.GoogleRole, DetailReceipt: receipts[id],
		})
	}
	return result, nil
}
