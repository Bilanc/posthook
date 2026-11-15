//go:build linux

package ingest

import (
	"os"
	"syscall"
	"time"
)

// changedAt is when a file last changed; see the darwin file.
func changedAt(info os.FileInfo) (at time.Time, ok bool) {
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return time.Time{}, false
	}
	at = time.Unix(int64(st.Ctim.Sec), int64(st.Ctim.Nsec))
	if modified := info.ModTime(); modified.After(at) {
		at = modified
	}
	return at, true
}
