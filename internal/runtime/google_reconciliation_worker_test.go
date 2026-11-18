package runtime

import (
	"database/sql"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"calendar-mcp/internal/calendar"
	googleprovider "calendar-mcp/internal/google"
	"calendar-mcp/internal/storage"
	"calendar-mcp/internal/token"
	"golang.org/x/oauth2"
)

type reconcileRoundTripper func(*http.Request) (*http.Response, error)

func (f reconcileRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGoogleWorkerUpdateNoDispatchAndUnknown(t *testing.T) {
	for _, scenario := range []string{"get-failure", "restore-failure", "prior-success", "etag-mismatch", "owner-mismatch", "type-invalid", "put-error", "put-lost", "commit-failure"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now().UTC()
			dbPath := filepath.Join(t.TempDir(), "worker.db")
			s, err := storage.Open(ctx, "sqlite://"+dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			if err = s.CreateConnection(ctx, storage.Connection{ID: "c", Provider: "google", AccountFingerprint: "c", Status: "connected", EncryptedCredentials: []byte("synthetic"), CredentialVersion: 1, ScopesJSON: "[]", CreatedAt: now, UpdatedAt: now}); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"google:source", "google:target"} {
				if err = s.UpsertCalendar(ctx, storage.Calendar{ID: id, ConnectionID: "c", ProviderCalendarID: strings.TrimPrefix(id, "google:"), Name: id, CanRead: true, CanWrite: true, DiscoveredAt: now}); err != nil {
					t.Fatal(err)
				}
			}
			rule := storage.Rule{ID: "rule", SourceCalendarID: "google:source", TargetCalendarID: "google:target", State: "enabled", IntervalSeconds: 600, LookaheadDays: 14, RecurrenceMode: "preserve", NotificationPolicy: "none", CreatedAt: now, UpdatedAt: now}
			if err = s.CreateRule(ctx, rule); err != nil {
				t.Fatal(err)
			}
			m := storage.Mapping{ID: "mapping", RuleID: rule.ID, ObjectKind: "cache_instance", SourceEventID: "event", TargetEventID: "copy", ContentHash: "old", ReconciliationState: "current", LastSeenAt: now}
			if err = s.UpsertMapping(ctx, m); err != nil {
				t.Fatal(err)
			}
			if scenario == "prior-success" {
				first := m
				first.ID = "first-mapping"
				first.SourceEventID = "a-event"
				first.TargetEventID = "copy-a"
				if err = s.UpsertMapping(ctx, first); err != nil {
					t.Fatal(err)
				}
			}
			seed, seedErr := s.ListMappings(ctx, rule.ID)
			if seedErr != nil || len(seed) == 0 {
				t.Fatalf("seed: %v", seedErr)
			}
			for i := range seed {
				seed[i].TargetETag = "target-etag"
				if err = s.UpdateMappingCAS(ctx, seed[i], seed[i].Version); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.ListMappings(ctx, rule.ID)
			if err != nil || len(before) != len(seed) {
				t.Fatal(err)
			}
			if scenario == "commit-failure" || scenario == "restore-failure" {
				db, dbErr := sql.Open("sqlite", dbPath)
				if dbErr != nil {
					t.Fatal(dbErr)
				}
				defer db.Close()
				if _, dbErr = db.ExecContext(ctx, `CREATE TRIGGER reject_mapping_commit BEFORE UPDATE ON event_mappings WHEN NEW.reconciliation_state='current' BEGIN SELECT RAISE(ABORT, 'synthetic commit failure'); END`); dbErr != nil {
					t.Fatal(dbErr)
				}
			}
			var methods []string
			oldTransport := http.DefaultTransport
			http.DefaultTransport = reconcileRoundTripper(func(r *http.Request) (*http.Response, error) {
				methods = append(methods, r.Method+" "+r.URL.Path)
				status := 200
				body := `{}`
				if strings.Contains(r.URL.Path, "/calendars/source/events") {
					body = `{"timeZone":"UTC","items":[{"id":"event","summary":"new title","status":"confirmed","start":{"date":"2026-11-18"},"end":{"date":"2026-11-19"}}]}`
					if scenario == "prior-success" {
						body = strings.Replace(body, `"items":[`, `"items":[{"id":"a-event","summary":"new title","status":"confirmed","start":{"date":"2026-11-18"},"end":{"date":"2026-11-19"}},`, 1)
					}
				} else if r.Method == http.MethodGet {
					if scenario == "get-failure" || scenario == "restore-failure" || (scenario == "prior-success" && !strings.HasSuffix(r.URL.Path, "copy-a")) {
						status = 403
						body = `{"error":{"code":403,"message":"private"}}`
					} else {
						etag, owner, eventType := "target-etag", "rule", "default"
						if scenario == "type-invalid" {
							eventType = "outOfOffice"
						}
						if scenario == "etag-mismatch" {
							etag = "different"
						}
						if scenario == "owner-mismatch" {
							owner = "different"
						}
						targetID, sourceID := "copy", "event"
						if strings.HasSuffix(r.URL.Path, "copy-a") {
							targetID = "copy-a"
							sourceID = "a-event"
						}
						body = `{"id":"` + targetID + `","etag":"` + etag + `","eventType":"` + eventType + `","status":"confirmed","organizer":{"self":true},"attendees":[],"extendedProperties":{"private":{"calendar_sync_rule":"` + owner + `","calendar_source_event":"` + sourceID + `"}},"start":{"date":"2026-11-18"},"end":{"date":"2026-11-19"}}`
					}
				} else {
					if r.URL.Query().Get("sendUpdates") != "none" || r.Header.Get("If-Match") != "target-etag" {
						t.Errorf("unsafe update options: %v", r.Header)
					}
					if scenario == "put-lost" {
						return nil, errors.New("synthetic lost response")
					}
					if scenario == "commit-failure" || scenario == "prior-success" {
						body = `{"id":"copy","etag":"new-etag","eventType":"default","status":"confirmed","organizer":{"self":true},"extendedProperties":{"private":{"calendar_sync_rule":"rule","calendar_source_event":"event"}},"start":{"date":"2026-11-18"},"end":{"date":"2026-11-19"}}`
						if scenario == "prior-success" {
							body = strings.ReplaceAll(strings.ReplaceAll(body, `"id":"copy"`, `"id":"copy-a"`), `"calendar_source_event":"event"`, `"calendar_source_event":"a-event"`)
						}
					} else {
						status = 412
						body = `{"error":{"code":412,"message":"private"}}`
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			defer func() { http.DefaultTransport = oldTransport }()
			provider, err := googleprovider.NewWithTokenStore("synthetic", "synthetic", token.NewFileStore(t.TempDir(), "google"), &oauth2.Token{AccessToken: "synthetic", Expiry: now.Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.EnqueueJob(ctx, storage.Job{ID: "job", RuleID: rule.ID, Kind: "manual", State: "pending", AvailableAt: now, CreatedAt: now}); err != nil {
				t.Fatal(err)
			}
			job, err := s.ClaimJob(ctx, "worker", now)
			if err != nil || job == nil {
				t.Fatalf("claim: %v", err)
			}
			err = executeJob(ctx, s, previewBuilder{providers: []calendar.Provider{provider}}, *job, now)
			if err == nil {
				t.Fatalf("expected failure, methods=%v", methods)
			}
			after, e := s.ListMappings(ctx, rule.ID)
			if e != nil || len(after) != len(before) {
				t.Fatalf("mapping: %v", e)
			}
			isPre := scenario == "get-failure" || scenario == "etag-mismatch" || scenario == "owner-mismatch" || scenario == "type-invalid"
			if scenario == "prior-success" {
				if after[0].ReconciliationState != "current" || after[0].ContentHash == before[0].ContentHash || after[1].ReconciliationState != "current" || after[1].ContentHash != before[1].ContentHash {
					t.Fatalf("prior success/restore: %+v", after)
				}
			} else if isPre {
				if after[0].ReconciliationState != "current" || after[0].ContentHash != before[0].ContentHash || after[0].Incarnation != before[0].Incarnation || after[0].Version != before[0].Version+2 {
					t.Fatalf("restore=%+v requests=%v err=%v", after[0], methods, err)
				}
			} else if after[0].ReconciliationState != "pending_update" {
				t.Fatalf("unknown not parked=%+v requests=%v err=%v", after[0], methods, err)
			}
			putCount := 0
			for _, method := range methods {
				if strings.HasPrefix(method, "PUT ") {
					putCount++
					wantID := "copy"
					if scenario == "prior-success" {
						wantID = "copy-a"
					}
					if method != "PUT /calendar/v3/calendars/target/events/"+wantID {
						t.Fatalf("unexpected update target: %s", method)
					}
				}
			}
			wantPut := 1
			if isPre || scenario == "restore-failure" {
				wantPut = 0
			}
			wantCalls := 3 + wantPut
			if scenario == "prior-success" {
				wantCalls = 5
			}
			if putCount != wantPut || len(methods) != wantCalls {
				t.Fatalf("unexpected request sequence: %v", methods)
			}
			runs, runErr := s.ListRuns(ctx, 10)
			if runErr != nil || len(runs) != 1 {
				t.Fatalf("run: %v", runErr)
			}
			if (isPre || scenario == "prior-success") && (runs[0].ErrorCode != "sync_target_pre_dispatch" || strings.Contains(runs[0].ErrorSummary, "private")) {
				t.Fatalf("unsafe diagnostic: %+v", runs[0])
			}
			if scenario == "restore-failure" && (runs[0].ErrorCode != "sync_target_pre_dispatch_unreconciled" || strings.Contains(runs[0].ErrorSummary, "synthetic")) {
				t.Fatalf("unsafe restore failure diagnostic: %+v", runs[0])
			}
			probe := storage.ExclusiveDispatchRequest{OperationID: "another-synthetic-run-0001", TargetKey: storage.GlobalSyncDispatchKey, RequestHash: strings.Repeat("0", 64), Kind: "run", CalendarIDs: []string{rule.TargetCalendarID}, Now: time.Now().UTC()}
			e = s.BeginExclusiveDispatch(ctx, probe)
			if isPre && e != nil {
				t.Fatalf("not released: %v, requests=%v", e, methods)
			}
			if !isPre && !errors.Is(e, storage.ErrExclusiveDispatchConflict) {
				t.Fatalf("not parked: %v, requests=%v", e, methods)
			}
		})
	}
}
