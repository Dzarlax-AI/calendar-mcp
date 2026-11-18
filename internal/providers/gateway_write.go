package providers

import (
	"context"
	"strings"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

// FindGatewayWriteReceipt builds only the original account and checks its
// authorization epoch on either side of the read. No retryable mutation
// request is reconstructed from the ledger.
func (f *Factory) FindGatewayWriteReceipt(ctx context.Context, w storage.GatewayWrite) (*calendar.EventV2, error) {
	provider, tail, ok := strings.Cut(w.Scope.CalendarID, ":")
	account, raw, canonical := strings.Cut(tail, ":")
	if !ok || !canonical || account == "" || raw == "" {
		return nil, storage.ErrAuthorityChanged
	}
	snapshot, err := f.store.CredentialSnapshot(ctx, account)
	if err != nil {
		return nil, err
	}
	if snapshot.Provider != provider || snapshot.Status != "connected" || snapshot.AuthorizationRevision != w.Scope.AuthorizationRevision {
		return nil, storage.ErrAuthorityChanged
	}
	var p calendar.Provider
	if provider == "google" {
		p, err = f.BuildGoogleVerification(snapshot)
	} else {
		p, err = f.BuildNonGoogleVerification(snapshot)
	}
	if err != nil {
		return nil, err
	}
	var event *calendar.EventV2
	if exact, ok := p.(calendar.GatewayPointWriteLookupProvider); ok && w.Scope.Kind == "update" {
		event, err = exact.FindGatewayPointWrite(ctx, calendar.EventRef{CalendarID: raw, EventID: w.Scope.EventID}, w.ID)
	} else if lookup, ok := p.(calendar.GatewayWriteLookupProvider); ok {
		event, err = lookup.FindGatewayWrite(ctx, raw, w.ID)
	}
	if err != nil || event == nil {
		return event, err
	}
	current, err := f.store.CredentialSnapshot(ctx, account)
	if err != nil {
		return nil, err
	}
	if current.Status != "connected" || current.AuthorizationRevision != w.Scope.AuthorizationRevision {
		return nil, storage.ErrAuthorityChanged
	}
	return event, nil
}
