package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"calendar-mcp/internal/storage"
)

type LegacyState struct {
	LastSync time.Time                `json:"last_sync"`
	Mappings map[string]LegacyMapping `json:"mappings"`
}

type LegacyMapping struct {
	GoogleID string `json:"google_id"`
	Hash     string `json:"hash"`
}

type Plan struct {
	Rule     storage.Rule
	Mappings []storage.Mapping
	LastSync time.Time
}

type Preview struct {
	RuleID       string    `json:"rule_id"`
	Source       string    `json:"source"`
	Target       string    `json:"target"`
	State        string    `json:"state"`
	MappingCount int       `json:"mapping_count"`
	LastSync     time.Time `json:"last_sync,omitempty"`
}

type Store interface {
	ImportLegacy(context.Context, storage.Rule, []storage.Mapping) error
}

func Load(stateFile, source, target string, now time.Time) (Plan, error) {
	if stateFile == "" {
		return Plan{}, errors.New("state file is required")
	}
	if !strings.HasPrefix(source, "microsoft:") || strings.TrimPrefix(source, "microsoft:") == "" {
		return Plan{}, errors.New("legacy source must be a non-empty microsoft calendar reference")
	}
	if !strings.HasPrefix(target, "google:") || strings.TrimPrefix(target, "google:") == "" {
		return Plan{}, errors.New("legacy target must be a non-empty google calendar reference")
	}
	if source == target {
		return Plan{}, errors.New("legacy source and target must differ")
	}
	data, err := os.ReadFile(stateFile)
	if err != nil {
		return Plan{}, fmt.Errorf("read legacy state: %w", err)
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return Plan{}, err
	}
	var state LegacyState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return Plan{}, fmt.Errorf("decode legacy state: %w", err)
	}
	if state.Mappings == nil {
		return Plan{}, errors.New("legacy state is missing mappings")
	}
	ruleID := deterministicID("legacy-rule", source, target)
	rule := storage.Rule{
		ID: ruleID, SourceCalendarID: source, TargetCalendarID: target, State: "paused",
		IntervalSeconds: 600, LookbackDays: 0, LookaheadDays: 14,
		RecurrenceMode: "preserve", NotificationPolicy: "none",
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
	sourceIDs := make([]string, 0, len(state.Mappings))
	for sourceID := range state.Mappings {
		sourceIDs = append(sourceIDs, sourceID)
	}
	sort.Strings(sourceIDs)
	targets := map[string]string{}
	mappings := make([]storage.Mapping, 0, len(sourceIDs))
	for _, sourceID := range sourceIDs {
		entry := state.Mappings[sourceID]
		if strings.TrimSpace(sourceID) == "" {
			return Plan{}, errors.New("legacy mapping has an empty source event id")
		}
		if strings.TrimSpace(entry.GoogleID) == "" {
			return Plan{}, fmt.Errorf("legacy mapping %q has no target event id", sourceID)
		}
		if previous, exists := targets[entry.GoogleID]; exists {
			return Plan{}, fmt.Errorf("legacy target event %q is mapped from both %q and %q", entry.GoogleID, previous, sourceID)
		}
		targets[entry.GoogleID] = sourceID
		if len(entry.Hash) != sha256.Size*2 {
			return Plan{}, fmt.Errorf("legacy mapping %q has an invalid content hash", sourceID)
		}
		if _, err := hex.DecodeString(entry.Hash); err != nil {
			return Plan{}, fmt.Errorf("legacy mapping %q has an invalid content hash", sourceID)
		}
		lastSeen := state.LastSync.UTC()
		if lastSeen.IsZero() {
			lastSeen = now.UTC()
		}
		mappings = append(mappings, storage.Mapping{
			ID: deterministicID("legacy-mapping", ruleID, sourceID), RuleID: ruleID,
			ObjectKind: "event", SourceEventID: sourceID, TargetEventID: entry.GoogleID,
			ContentHash: entry.Hash, LastSeenAt: lastSeen, ReconciliationState: "legacy",
		})
	}
	return Plan{Rule: rule, Mappings: mappings, LastSync: state.LastSync.UTC()}, nil
}

func (p Plan) Preview() Preview {
	return Preview{
		RuleID: p.Rule.ID, Source: p.Rule.SourceCalendarID, Target: p.Rule.TargetCalendarID,
		State: p.Rule.State, MappingCount: len(p.Mappings), LastSync: p.LastSync,
	}
}

func Import(ctx context.Context, store Store, plan Plan) error {
	if store == nil {
		return errors.New("legacy import store is required")
	}
	if err := storage.ValidateRule(plan.Rule); err != nil {
		return err
	}
	if plan.Rule.State != "paused" {
		return errors.New("legacy rule must be imported paused")
	}
	return store.ImportLegacy(ctx, plan.Rule, plan.Mappings)
}

func deterministicID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := walkJSONValue(decoder); err != nil {
		return fmt.Errorf("validate legacy state: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("validate trailing JSON: %w", err)
		}
		return errors.New("validate legacy state: trailing JSON value")
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if keys[key] {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			keys[key] = true
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := walkJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}
