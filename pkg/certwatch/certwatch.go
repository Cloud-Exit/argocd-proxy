package certwatch

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"os"
	"time"
)

// PollInterval is how often certificate files are checked for changes.
const PollInterval = 10 * time.Second

// Watch polls the given files for content changes. When any file's content
// differs from its initial snapshot, cancelFn is called to trigger a graceful
// restart. Files that don't exist at startup are ignored (they may appear
// later, but we only care about rotation of files that were loaded).
//
// Watch blocks until ctx is cancelled.
func Watch(ctx context.Context, logger *slog.Logger, cancelFn context.CancelFunc, paths ...string) {
	// Take an initial snapshot of all files that exist.
	snapshots := make(map[string][sha256.Size]byte)
	for _, p := range paths {
		if p == "" {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue // file doesn't exist at startup, skip
		}
		snapshots[p] = sha256.Sum256(data)
	}

	if len(snapshots) == 0 {
		return // nothing to watch
	}

	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for path, oldHash := range snapshots {
				data, err := os.ReadFile(path)
				if err != nil {
					// File disappeared — treat as a change.
					logger.Warn("certificate file disappeared, triggering restart", "path", path)
					cancelFn()
					return
				}
				newHash := sha256.Sum256(data)
				if newHash != oldHash {
					logger.Info("certificate file changed, triggering restart", "path", path)
					cancelFn()
					return
				}
			}
		}
	}
}
