package mcpserver

import (
	"encoding/json"
	"testing"

	"calendar-mcp/internal/application"
	"calendar-mcp/internal/calendar"
)

func TestBuildServerRegistersV2ToolsOnlyWhenEnabled(t *testing.T) {
	reg := calendar.NewRegistry(nil)
	app := application.New(reg)

	disabled := buildServer(reg, app, false)
	if disabled.GetTool("get_events_v2") != nil {
		t.Fatal("get_events_v2 registered while ENABLE_V2 is false")
	}

	enabled := buildServer(reg, app, true)
	for _, name := range []string{"get_calendar_capabilities", "get_event_v2", "get_events_v2", "create_event_v2", "update_event_v2", "delete_event_v2", "get_event_instances_v2", "search_events_v2", "respond_to_event_v2", "move_event_v2", "import_event_v2"} {
		tool := enabled.GetTool(name)
		if tool == nil {
			t.Fatalf("tool %q was not registered", name)
		}
		if len(tool.Tool.RawInputSchema) == 0 {
			t.Fatalf("tool %q has no raw input schema", name)
		}
		var schema map[string]any
		if err := json.Unmarshal(tool.Tool.RawInputSchema, &schema); err != nil {
			t.Fatalf("tool %q schema is invalid JSON: %v", name, err)
		}
		if schema["type"] != "object" {
			t.Fatalf("tool %q schema type = %#v, want object", name, schema["type"])
		}
	}
}
