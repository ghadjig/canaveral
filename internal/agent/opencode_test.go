package agent

import "testing"

func TestRelevantEventFiltersTheFirehose(t *testing.T) {
	want := []string{
		"session.idle", "session.error", "session.status",
		"permission.asked", "permission.v2.asked",
		"question.asked", "question.v2.asked", "question.v2.replied",
		// Todo progress moves a progress gauge even while the headline
		// status stays "working".
		"todo.updated",
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
		"message.part.delta", "message.part.updated", "session.next.text.delta",
		"session.next.reasoning.delta", "plugin.added", "server.heartbeat",
		"file.watcher.updated",
	}
	for _, e := range ignore {
		if relevantEvent(e) {
			t.Errorf("relevantEvent(%q) = true, want false", e)
		}
	}
}
