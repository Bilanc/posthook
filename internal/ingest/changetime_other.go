//go:build !darwin && !linux

package ingest

import (
	"os"
	"time"
)

// changedAt is when a file last changed; see the darwin file. Where the
// status change time cannot be read the answer is "not known", and a look
// reads the file every time.
func changedAt(os.FileInfo) (at time.Time, ok bool) { return time.Time{}, false }
