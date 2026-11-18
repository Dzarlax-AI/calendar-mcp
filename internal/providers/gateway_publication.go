package providers

import (
	"context"
	"net/http"
	"strings"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/storage"
)

// BuildGatewayGooglePublication observes authorization only for the strict
// publication identified by ticket. It never installs the live cache observers.
func (f *Factory) BuildGatewayGooglePublication(ctx context.Context, ticket storage.GatewayPublicationTicket) (calendar.Provider, string, error) {
	grant := ticket.Grant
	id := strings.SplitN(grant.Metadata.ID, ":", 3)
	role := storage.DetailReceiptRole(grant.DetailReceipt)
	if len(id) != 3 || id[0] != "google" || id[1] == "" || id[2] == "" || role == "" {
		return nil, "", storage.ErrAuthorityChanged
	}
	snapshot, err := f.store.CredentialSnapshot(ctx, id[1])
	if err != nil {
		return nil, "", err
	}
	if snapshot.Provider != "google" || snapshot.Status != "connected" || snapshot.AuthorizationRevision != grant.ConnectionRevision {
		return nil, "", storage.ErrAuthorityChanged
	}
	provider, err := f.buildGoogleVerification(snapshot)
	if err != nil {
		return nil, "", err
	}
	closeProof := func(ctx context.Context, revision string, digest [32]byte) error {
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return f.store.InvalidateGatewayGooglePublication(finish, snapshot.ConnectionID, grant, revision, digest)
	}
	provider.SetRoleObserver(func(_ context.Context, calendarID string) (func(context.Context, string, string, [32]byte) error, error) {
		if calendarID != id[2] {
			return nil, storage.ErrAuthorityChanged
		}
		return func(ctx context.Context, observed, revision string, digest [32]byte) error {
			if revision == grant.ConnectionRevision && observed == role {
				return nil
			}
			if err := closeProof(ctx, revision, digest); err != nil {
				return err
			}
			return storage.ErrAuthorityChanged
		}, nil
	})
	provider.SetDenialObserver(func(_ context.Context, calendarID string) (func(context.Context, string, int, string, string, [32]byte) error, error) {
		if calendarID != id[2] {
			return nil, storage.ErrAuthorityChanged
		}
		return func(ctx context.Context, operation string, status int, reason, revision string, digest [32]byte) error {
			if status == http.StatusUnauthorized || status == http.StatusNotFound && operation == "event_list" ||
				status == http.StatusForbidden && !googleQuotaDenial(reason) {
				return closeProof(ctx, revision, digest)
			}
			return nil
		}, nil
	})
	return provider, id[2], nil
}
