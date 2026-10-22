package agent

import "testing"

func TestRelevantEventFiltersTheFirehose(t *testing.T) {
	want := []string{
		"session.execution.started", "session.execution.succeeded",
		"session.execution.failed", "session.execution.interrupted",
		"session.retry.scheduled",
		"permission.asked", "permission.replied",
		"form.created", "form.replied", "form.cancelled",
		// Todo progress moves a progress gauge even while the headline
		// status stays "working", and arrives as a finished tool call.
		"session.tool.success",
		"server.connected",
	}
	for _, e := range want {
		if !relevantEvent(e) {
			t.Errorf("relevantEvent(%q) = false, want true", e)
		}
	}
	// The high-frequency streaming events must be ignored, or every token
	// would trigger a re-probe of every agent.
	ignore := []string{
		"session.text.delta", "session.reasoning.delta", "session.tool.input.delta",
		"session.tool.progress", "project.updated", "server.heartbeat",
		"file.watcher.updated",
	}
	for _, e := range ignore {
		if relevantEvent(e) {
			t.Errorf("relevantEvent(%q) = true, want false", e)
		}
	}
}
