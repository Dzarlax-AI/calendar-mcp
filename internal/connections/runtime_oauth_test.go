package connections

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	"calendar-mcp/internal/credentials"
	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/token"
	"golang.org/x/oauth2"
)

type runtimeOAuthTransport func(*http.Request) (*http.Response, error)

func (f runtimeOAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func runtimeOAuthFixture(t *testing.T, expiry time.Time) (*storage.Store, *Service, storage.CredentialSnapshot) {
	t.Helper()
	ctx := context.Background()
	s, err := storage.Open(ctx, "sqlite://"+filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := credentials.NewCipher(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	svc := New(s, c)
	if err = svc.CreateOAuth(ctx, "synthetic", "google", "Synthetic", &oauth2.Token{AccessToken: "old-access", RefreshToken: "old-refresh", Expiry: expiry}, nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snap, err := s.BeginAuthorization(ctx, "synthetic", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	id := calendar.AccountCalendarID("google", "synthetic", "cal")
	if err = s.CompleteGoogleAuthorization(ctx, snap, []storage.DiscoveredDetailCalendar{{Calendar: storage.Calendar{ID: id, ConnectionID: "synthetic", ProviderCalendarID: "cal", Name: "Synthetic"}, AccessRole: "owner"}}, now); err != nil {
		t.Fatal(err)
	}
	snap, err = s.CredentialSnapshot(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	return s, svc, snap
}
func runtimeOAuthSource(t *testing.T, svc *Service, snap storage.CredentialSnapshot) oauth2.TokenSource {
	t.Helper()
	st := svc.SnapshotOAuthTokenStore(snap)
	initial, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &oauth2.Config{ClientID: "synthetic", ClientSecret: "synthetic", Endpoint: oauth2.Endpoint{TokenURL: "https://oauth2.googleapis.com/token", AuthStyle: oauth2.AuthStyleInParams}}
	return token.TokenSource(st, cfg, initial)
}
func runtimeOAuthWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("synthetic request timed out")
	}
}
func runtimeOAuthInstall(t *testing.T, f runtimeOAuthTransport) {
	t.Helper()
	old := http.DefaultTransport
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = old })
}
func runtimeOAuthResponse(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestRuntimeOAuthFirstTokenNoopPreservesAuthority(t *testing.T) {
	ctx := context.Background()
	s, svc, snap := runtimeOAuthFixture(t, time.Now().Add(time.Hour))
	before, err := s.ConnectionByID(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	sources, err := s.ListCalendars(ctx, "synthetic")
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources: %v %v", sources, err)
	}
	runtimeOAuthInstall(t, func(r *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("unexpected network request: %s", r.URL.Host)
	})
	tok, err := runtimeOAuthSource(t, svc, snap).Token()
	if err != nil || tok.AccessToken != "old-access" {
		t.Fatalf("token=%v err=%v", tok, err)
	}
	after, err := s.ConnectionByID(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	actual, err := s.ListCalendars(ctx, "synthetic")
	if err != nil || len(actual) != 1 {
		t.Fatalf("sources: %v %v", actual, err)
	}
	if !bytes.Equal(before.EncryptedCredentials, after.EncryptedCredentials) || before.AuthorizationRevision != after.AuthorizationRevision || !before.UpdatedAt.Equal(after.UpdatedAt) || sources[0].DetailReceipt != actual[0].DetailReceipt || !actual[0].CanRead {
		t.Fatal("first Token changed credentials or authorization")
	}
}

func TestRuntimeOAuthConcurrentRefreshCAS(t *testing.T) {
	ctx := context.Background()
	s, svc, snap := runtimeOAuthFixture(t, time.Now().Add(-time.Hour))
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	runtimeOAuthInstall(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "oauth2.googleapis.com" {
			return nil, fmt.Errorf("blocked host")
		}
		started <- struct{}{}
		<-release
		return runtimeOAuthResponse(r, `{"access_token":"fresh","refresh_token":"rotated","token_type":"Bearer","expires_in":3600}`), nil
	})
	a, b := runtimeOAuthSource(t, svc, snap), runtimeOAuthSource(t, svc, snap)
	done := make(chan error, 2)
	go func() { _, e := a.Token(); done <- e }()
	go func() { _, e := b.Token(); done <- e }()
	runtimeOAuthWait(t, started)
	runtimeOAuthWait(t, started)
	close(release)
	e1, e2 := <-done, <-done
	if (e1 == nil) == (e2 == nil) {
		t.Fatalf("expected one CAS winner, got %v and %v", e1, e2)
	}
	stored, err := svc.OAuthTokenStore("synthetic", "google").Load()
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "fresh" || stored.RefreshToken != "rotated" {
		t.Fatalf("unexpected stored token")
	}
	rec, err := s.ConnectionByID(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	sources, err := s.ListCalendars(ctx, "synthetic")
	if err != nil || len(sources) != 1 || !sources[0].CanRead || rec.AuthorizationRevision != snap.AuthorizationRevision {
		t.Fatalf("authorization changed: %v", err)
	}
}

func TestRuntimeOAuthDelayedRefreshCannotOverwriteReconnect(t *testing.T) {
	ctx := context.Background()
	s, svc, snap := runtimeOAuthFixture(t, time.Now().Add(-time.Hour))
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	runtimeOAuthInstall(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "oauth2.googleapis.com" {
			return nil, fmt.Errorf("blocked host")
		}
		started <- struct{}{}
		<-release
		return runtimeOAuthResponse(r, `{"access_token":"old-fresh","refresh_token":"old-rotated","expires_in":3600}`), nil
	})
	source := runtimeOAuthSource(t, svc, snap)
	done := make(chan error, 1)
	go func() { _, e := source.Token(); done <- e }()
	runtimeOAuthWait(t, started)
	if err := svc.ReconnectOAuth(ctx, "synthetic", "google", &oauth2.Token{AccessToken: "replacement", RefreshToken: "replacement-refresh", Expiry: time.Now().Add(time.Hour)}); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("stale refresh succeeded")
	}
	stored, err := svc.OAuthTokenStore("synthetic", "google").Load()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.ConnectionByID(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "replacement" || stored.RefreshToken != "replacement-refresh" || rec.AuthorizationRevision == snap.AuthorizationRevision {
		t.Fatal("old refresh clobbered reconnect")
	}
}

func TestRuntimeOAuthDelayedRefreshCannotOverwriteRecreatedConnection(t *testing.T) {
	ctx := context.Background()
	s, svc, snap := runtimeOAuthFixture(t, time.Now().Add(-time.Hour))
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	runtimeOAuthInstall(t, func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "oauth2.googleapis.com" {
			return nil, fmt.Errorf("blocked host")
		}
		started <- struct{}{}
		<-release
		return runtimeOAuthResponse(r, `{"access_token":"old-fresh","refresh_token":"old-rotated","expires_in":3600}`), nil
	})
	source := runtimeOAuthSource(t, svc, snap)
	done := make(chan error, 1)
	go func() { _, e := source.Token(); done <- e }()
	runtimeOAuthWait(t, started)
	if err := svc.Delete(ctx, "synthetic"); err != nil {
		close(release)
		t.Fatal(err)
	}
	if err := svc.CreateOAuth(ctx, "synthetic", "google", "Recreated", &oauth2.Token{AccessToken: "recreated", RefreshToken: "recreated-refresh", Expiry: time.Now().Add(time.Hour)}, nil); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("stale refresh succeeded")
	}
	stored, err := svc.OAuthTokenStore("synthetic", "google").Load()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.ConnectionByID(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != "recreated" || stored.RefreshToken != "recreated-refresh" || rec.AuthorizationRevision == snap.AuthorizationRevision {
		t.Fatal("old refresh clobbered recreated account")
	}
}
func TestRuntimeOAuthReconnectMissingRefreshDoesNotTouchCurrentGrant(t *testing.T) {
	ctx := context.Background()
	s, svc, _ := runtimeOAuthFixture(t, time.Now().Add(time.Hour))
	before, err := s.ConnectionByID(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	sources, err := s.ListCalendars(ctx, "synthetic")
	if err != nil || len(sources) != 1 {
		t.Fatal(err)
	}
	if err = svc.ReconnectOAuth(ctx, "synthetic", "google", &oauth2.Token{AccessToken: "other-account"}); err == nil {
		t.Fatal("accepted token without its own refresh credential")
	}
	after, err := s.ConnectionByID(ctx, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.ListCalendars(ctx, "synthetic")
	if err != nil || len(current) != 1 {
		t.Fatal(err)
	}
	if !bytes.Equal(before.EncryptedCredentials, after.EncryptedCredentials) || before.AuthorizationRevision != after.AuthorizationRevision || before.Status != after.Status || !before.UpdatedAt.Equal(after.UpdatedAt) || sources[0].DetailReceipt != current[0].DetailReceipt || !current[0].CanRead {
		t.Fatal("rejected reconnect changed current authorization")
	}
}
