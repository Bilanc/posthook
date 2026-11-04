package ingest

import "github.com/bilanc/posthook/internal/paths"

// vscodeUserDirsForTest exposes the platform VS Code user dirs so the
// discovery test can plant a transcript where production code will look.
func vscodeUserDirsForTest() []string { return paths.VSCodeUserDirs() }
