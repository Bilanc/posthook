package installers

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bilanc/posthook/internal/atomicfs"
	"github.com/bilanc/posthook/internal/paths"
)

const piAgentSlug = "pi"

// piExtensionTemplate is the Pi extension posthook installs, with
// __POSTHOOK_BIN__ standing for the binary path as a JSON string literal
// (also a valid TypeScript one).
//
//go:embed pi_extension.ts
var piExtensionTemplate string

// DetectPi reports whether Pi (pi.dev) has been used on this machine: Pi
// creates its agent directory (~/.pi/agent, or PI_CODING_AGENT_DIR) on first
// launch.
func DetectPi() bool {
	_, err := os.Stat(paths.PiAgentDir())
	return err == nil
}

func renderPiExtension(binaryPath string) (string, error) {
	if !isPosthookPiExtension(piExtensionTemplate) {
		return "", fmt.Errorf("pi_extension.ts must start with %q", piExtensionHeader)
	}
	quoted, err := json.Marshal(binaryPath)
	if err != nil {
		return "", err
	}
	return strings.Replace(piExtensionTemplate, "__POSTHOOK_BIN__", string(quoted), 1), nil
}

// piExtensionHeader opens every extension posthook writes (pi_extension.ts),
// whatever the release or binary path. Ownership is decided by this exact
// header, not by the file merely mentioning posthook: a posthook.ts the user
// wrote must never be replaced or deleted.
const piExtensionHeader = "// posthook ingest --agent pi\n//\n// Installed by `posthook init`"

// isPosthookPiExtension reports whether content is an extension posthook
// wrote.
func isPosthookPiExtension(content string) bool {
	return strings.HasPrefix(content, piExtensionHeader)
}

// PiExtensionInstalled reports whether posthook's own Pi extension is in
// place.
func PiExtensionInstalled() bool {
	data, err := os.ReadFile(paths.PiExtensionPath())
	return err == nil && isPosthookPiExtension(string(data))
}

// InstallPiExtension writes posthook's extension to
// <agent dir>/extensions/posthook.ts. Pi has no hook configuration: an
// extension subscribes to Pi's events instead, and Pi loads every *.ts file in
// that directory on start, so posthook owns this one file outright and
// rewrites it whenever its content (or the binary path) changes. A
// posthook.ts that posthook did not write is left alone and reported.
func InstallPiExtension(binaryPath string) (Result, error) {
	path := paths.PiExtensionPath()
	want, err := renderPiExtension(binaryPath)
	if err != nil {
		return Result{}, err
	}
	current, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return Result{}, err
	case string(current) == want:
		return Result{Path: path, Message: "Pi: already up to date"}, nil
	case !isPosthookPiExtension(string(current)):
		return Result{}, fmt.Errorf("%s exists and was not written by posthook; leaving it alone", path)
	}
	if err := atomicfs.Write(path, []byte(want), 0o644); err != nil {
		return Result{}, err
	}
	return Result{Changed: true, Path: path, Message: fmt.Sprintf("Pi: extension installed at %s (takes effect in new Pi sessions, or after /reload)", path)}, nil
}

// UninstallPiExtension deletes the extension posthook installed. A missing
// file, or one posthook did not write, is left as it is.
func UninstallPiExtension() (Result, error) {
	path := paths.PiExtensionPath()
	current, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Result{Path: path, Message: "Pi: no extension, nothing to remove"}, nil
	}
	if err != nil {
		return Result{}, err
	}
	if !isPosthookPiExtension(string(current)) {
		return Result{Path: path, Message: fmt.Sprintf("Pi: %s was not written by posthook; left alone", path)}, nil
	}
	if err := os.Remove(path); err != nil {
		return Result{}, err
	}
	return Result{Changed: true, Path: path, Message: fmt.Sprintf("Pi: deleted %s", path)}, nil
}
