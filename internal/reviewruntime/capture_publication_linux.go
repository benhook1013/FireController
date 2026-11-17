//go:build linux

package reviewruntime

import (
	"context"
	"github.com/benhook1013/FireController/internal/capturecustody"
	"time"
)

func capturePublication(ctx context.Context, root string) (publicationCustody, error) {
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return capturecustody.Writer(wait, root)
}
func captureStagingPattern(kind string) string { return ".capture-stage-v1-" + kind + "-*" }
