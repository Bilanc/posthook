package installers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bilanc/posthook/internal/paths"
)

func TestDetectPi(t *testing.T) {
	home := isolate(t)
	if DetectPi() {
		t.Fatal("detected Pi with no ~/.pi/agent")
	}
	if err := os.MkdirAll(filepath.Join(home, ".pi", "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !DetectPi() {
		t.Fatal("did not detect ~/.pi/agent")
	}
}

func TestPiAgentDirHonoursEnv(t *testing.T) {
	home := isolate(t)
	t.Setenv("PI_CODING_AGENT_DIR", "~/pi-config")
	if got, want := paths.PiExtensionPath(), filepath.Join(home, "pi-config", "extensions", "posthook.ts"); got != want {
		t.Errorf("PiExtensionPath = %s, want %s", got, want)
	}
	t.Setenv("PI_CODING_AGENT_DIR", "/srv/pi")
	if got := paths.PiExtensionPath(); got != "/srv/pi/extensions/posthook.ts" {
		t.Errorf("PiExtensionPath = %s", got)
	}
}

func TestInstallPiExtension(t *testing.T) {
	isolate(t)
	bin := `/opt/my "odd" bin/posthook`
	res, err := InstallPiExtension(bin)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.Path != paths.PiExtensionPath() {
		t.Fatalf("result = %+v", res)
	}
	data, err := os.ReadFile(paths.PiExtensionPath())
	if err != nil {
		t.Fatal(err)
	}
	src := string(data)
	quoted, _ := json.Marshal(bin)
	if !strings.Contains(src, "const POSTHOOK_BIN: string = "+string(quoted)+";") {
		t.Errorf("binary path not written as a string literal:\n%s", src)
	}
	if strings.Contains(src, "__POSTHOOK_BIN__") {
		t.Error("placeholder left in the installed extension")
	}
	if !IsPosthookCommand(src, "pi") {
		t.Error("installed file does not carry the posthook marker uninstall looks for")
	}

	again, err := InstallPiExtension(bin)
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed {
		t.Error("second install rewrote an up-to-date extension")
	}

	moved, err := InstallPiExtension("/usr/local/bin/posthook")
	if err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(paths.PiExtensionPath())
	if !moved.Changed || !strings.Contains(string(data), `"/usr/local/bin/posthook"`) {
		t.Error("install did not follow the binary to its new path")
	}
}

// A posthook.ts that posthook did not write is the user's: never replace or
// delete it.
func TestPiExtensionLeavesSomeoneElsesFileAlone(t *testing.T) {
	isolate(t)
	mine := "export default function (pi) {}\n"
	mustWrite(t, paths.PiExtensionPath(), mine)

	if _, err := InstallPiExtension(testBin); err == nil {
		t.Error("install replaced a posthook.ts posthook did not write")
	}
	if res, err := UninstallPiExtension(); err != nil || res.Changed {
		t.Errorf("uninstall = %+v, %v; want it to leave the file", res, err)
	}
	if data, _ := os.ReadFile(paths.PiExtensionPath()); string(data) != mine {
		t.Errorf("file changed to:\n%s", data)
	}
}

func TestUninstallPiExtension(t *testing.T) {
	isolate(t)
	if res, err := UninstallPiExtension(); err != nil || res.Changed {
		t.Fatalf("uninstall with nothing installed = %+v, %v", res, err)
	}
	if _, err := InstallPiExtension(testBin); err != nil {
		t.Fatal(err)
	}
	res, err := UninstallPiExtension()
	if err != nil || !res.Changed {
		t.Fatalf("uninstall = %+v, %v", res, err)
	}
	if _, err := os.Stat(paths.PiExtensionPath()); !os.IsNotExist(err) {
		t.Errorf("extension still there: %v", err)
	}
}
