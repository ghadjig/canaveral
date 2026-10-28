package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/bandito/canaveral/internal/agent"
)

// runClaudeHook is what the hooks canaveral starts Claude Code with run. It
// is not meant to be typed: the hook event arrives as JSON on stdin.
func runClaudeHook(_ context.Context, args []string) error {
	if len(args) > 0 {
		fmt.Fprintln(os.Stderr, "Usage: canaveral "+agent.ClaudeHookCommand+" < event.json\n\n"+
			"Record a Claude Code hook event for `canaveral watch`. Claude Code runs\n"+
			"this itself, from the hooks canaveral starts its agents with.")
		return fmt.Errorf("takes no arguments")
	}
	return agent.RecordClaudeHook(os.Stdin)
}
