package atomicfs

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Write writes content to path atomically: temp file in the same directory
// + rename. The parent directory is created if missing.
func Write(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.%d.tmp", path, os.Getpid(), time.Now().UnixNano())
	if err := os.WriteFile(tmp, content, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// WriteString is a convenience wrapper for textual content with 0o644 perms.
func WriteString(path, content string) error {
	return Write(path, []byte(content), 0o644)
}
