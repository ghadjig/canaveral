package cli

import (
	"context"
	"crypto/rand"
	"fmt"
)

func runScratch(ctx context.Context, args []string) error {
	return openFeature(ctx, "scratch", args, true, false)
}

func scratchName() (string, error) {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate scratch name: %w", err)
	}
	return fmt.Sprintf("scratch-%x", id), nil
}
