//go:build darwin

package ingest

import (
	"os"
	"syscall"
	"time"
)

// changedAt is when a file last changed: its content, or the file that
// stands under its name. That is the later of its modification time and its
// status change time. The modification time alone does not do: `cp -p`,
// `mv`, `rsync -a` and unpacking an archive put new content in place under
// an old one, and none of them can set the status change time. ok is false
// when the system does not say.
func changedAt(info os.FileInfo) (at time.Time, ok bool) {
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return time.Time{}, false
	}
	at = time.Unix(int64(st.Ctimespec.Sec), int64(st.Ctimespec.Nsec))
	if modified := info.ModTime(); modified.After(at) {
		at = modified
	}
	return at, true
}
