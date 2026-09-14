package hypr

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// ensurePlacementRule matches the window rather than the exec process. Browsers
// can hand startup to another process, losing Hyprland's PID-scoped exec rule.
// Install before spawning so even those windows map silently on their workspace.
func ensurePlacementRule(ctx context.Context, class, workspace string) error {
	if class == "" || workspace == "" || strings.ContainsAny(class+workspace, ",;\n\r") {
		return fmt.Errorf("invalid window class or workspace for placement rule")
	}
	match := "^" + regexp.QuoteMeta(class) + "$"
	modern := "match:initial_class " + match + ", workspace name:" + workspace + " silent, no_initial_focus on"
	modernErr := windowRule(ctx, "windowrule", modern)
	if modernErr == nil {
		return nil
	}
	// Hyprland 0.53 replaced windowrulev2 with the match:/effect syntax.
	// Trying the grammar handles both releases and development builds without
	// guessing from version strings. Check stdout too: hyprctl can exit zero
	// while reporting a rejected rule there.
	legacy := "workspace name:" + workspace + " silent,initialclass:" + match
	if err := windowRule(ctx, "windowrulev2", legacy); err != nil {
		return fmt.Errorf("install placement rule: %v; legacy syntax: %w", modernErr, err)
	}
	return windowRule(ctx, "windowrulev2", "noinitialfocus,initialclass:"+match)
}

func windowRule(ctx context.Context, keyword, rule string) error {
	out, err := exec.CommandContext(ctx, "hyprctl", "keyword", keyword, rule).CombinedOutput()
	if err != nil {
		return fmt.Errorf("install %s: %w: %s", keyword, err, strings.TrimSpace(string(out)))
	}
	if response := strings.TrimSpace(string(out)); response != "" && response != "ok" {
		return fmt.Errorf("install %s: %s", keyword, response)
	}
	return nil
}
