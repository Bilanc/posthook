package proxy

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bilanc/posthook/internal/notes"
)

func TestSplitGitInvocation(t *testing.T) {
	// Git accepts global options before its subcommand. Posthook must skip those
	// options and their values to decide whether this invocation needs commit or
	// clone capture. It also returns only the arguments after the subcommand so
	// clone destination parsing never sees the preceding global options.
	tests := []struct {
		name                string
		givenArgs           []string
		expectedSubcommand  string
		expectedCommandArgs []string
	}{
		{
			// Preserve the ordinary terminal invocation that already worked.
			name:                "plain commit",
			givenArgs:           []string{"commit", "--quiet"},
			expectedSubcommand:  "commit",
			expectedCommandArgs: []string{"--quiet"},
		},
		{
			// Regression case: Cursor inserts this per-command config override,
			// which previously made Posthook mistake "-c" for the subcommand.
			name:                "Cursor commit with config override",
			givenArgs:           []string{"-c", "user.useConfigOnly=true", "commit", "--quiet", "--file", "-"},
			expectedSubcommand:  "commit",
			expectedCommandArgs: []string{"--quiet", "--file", "-"},
		},
		{
			// `-C` tells Git which directory to use. Clone must still receive only
			// its repository and destination arguments.
			name:                "clone with working directory",
			givenArgs:           []string{"-C", "/tmp", "clone", "repo", "destination"},
			expectedSubcommand:  "clone",
			expectedCommandArgs: []string{"repo", "destination"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Given: arguments passed to Posthook's Git shadow.
			givenArgs := tt.givenArgs

			// When: Posthook separates the Git command from its arguments.
			actualSubcommand, actualCommandArgs := splitGitInvocation(givenArgs)

			// Then: it returns the expected command and command arguments.
			if actualSubcommand != tt.expectedSubcommand {
				t.Fatalf("subcommand = %q, want %q", actualSubcommand, tt.expectedSubcommand)
			}
			if !reflect.DeepEqual(actualCommandArgs, tt.expectedCommandArgs) {
				t.Fatalf("command args = %#v, want %#v", actualCommandArgs, tt.expectedCommandArgs)
			}
		})
	}
}

func TestRemoteFromArgs(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		flags map[string]bool
		want  string
	}{
		{"bare push lets git choose", []string{}, pushFlagsWithValues, ""},
		{"push origin main", []string{"origin", "main"}, pushFlagsWithValues, "origin"},
		{"push -u origin main", []string{"-u", "origin", "main"}, pushFlagsWithValues, "origin"},
		{"push --force-with-lease origin", []string{"--force-with-lease", "origin"}, pushFlagsWithValues, "origin"},
		{"push -o value skips the option value", []string{"-o", "ci.skip", "upstream"}, pushFlagsWithValues, "upstream"},
		{"push --repo=", []string{"--repo=fork", "main"}, pushFlagsWithValues, "fork"},
		{"push after --", []string{"--", "fork"}, pushFlagsWithValues, "fork"},
		{"fetch --depth 1 origin", []string{"--depth", "1", "origin"}, fetchFlagsWithValues, "origin"},
		{"pull --rebase origin main", []string{"--rebase", "origin", "main"}, fetchFlagsWithValues, "origin"},
		{"pull -X theirs", []string{"-X", "theirs", "upstream"}, fetchFlagsWithValues, "upstream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := remoteFromArgs(tt.args, tt.flags); got != tt.want {
				t.Fatalf("remoteFromArgs(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestPushSkipsNotes(t *testing.T) {
	skip := [][]string{
		{"--dry-run"}, {"-n", "origin"}, {"origin", "--delete", "feature"},
		{"--mirror"}, {"--tags"}, {"origin", "refs/notes/posthook:refs/notes/posthook"},
	}
	for _, args := range skip {
		if !pushSkipsNotes(args) {
			t.Errorf("expected %v to skip notes transport", args)
		}
	}
	run := [][]string{{}, {"origin", "main"}, {"-u", "origin", "HEAD"}, {"--force-with-lease", "origin", "main"}}
	for _, args := range run {
		if pushSkipsNotes(args) {
			t.Errorf("expected %v to run notes transport", args)
		}
	}
}

func TestFetchHasExplicitRefspec(t *testing.T) {
	explicit := [][]string{{"origin", "main"}, {"--rebase", "origin", "main"}, {"-q", "origin", "main:main"}, {"--depth", "1", "origin", "main"}}
	for _, args := range explicit {
		if !fetchHasExplicitRefspec(args) {
			t.Errorf("expected %v to count as an explicit refspec", args)
		}
	}
	bare := [][]string{{}, {"origin"}, {"--all"}, {"-q", "origin"}, {"--rebase"}, {"--depth", "1", "origin"}}
	for _, args := range bare {
		if fetchHasExplicitRefspec(args) {
			t.Errorf("expected %v to use configured refspecs", args)
		}
	}
}

// runGit runs real git in dir (POSTHOOK_BYPASS=1 so a shadowed `git` on the
// test machine passes straight through) and returns its combined output.
func runGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"POSTHOOK_BYPASS=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x",
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// brokenClone returns a clone whose origin has no notes ref and whose config
// carries the exact notes refspec posthook <= 0.3.0 wrote, which makes the
// user's own `git fetch` / `git pull` fail.
func brokenClone(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	mustGit(t, root, "init", "-q", "--bare", "-b", "main", remote)
	mustGit(t, root, "clone", "-q", remote, repo)
	mustGit(t, repo, "commit", "-q", "--allow-empty", "-m", "one")
	mustGit(t, repo, "push", "-q", "origin", "HEAD:main")
	mustGit(t, repo, "config", "--add", "remote.origin.fetch", brokenRefspec)
	if out, err := runGit(t, repo, "fetch", "-q", "origin"); err == nil {
		t.Fatalf("expected the broken refspec to make fetch fail\n%s", out)
	}
	return repo
}

const brokenRefspec = "+refs/notes/posthook:refs/notes/posthook-remote"

func chdir(t *testing.T, dir string) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

// Real git fails before any post-success handler can run, so the shadow has
// to repair the config it inherited from an earlier release *before* git.
func TestPreflightRepairsBrokenNotesRefspecSoFetchSucceeds(t *testing.T) {
	repo := brokenClone(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	chdir(t, repo)

	preflight(realGit, "pull", nil)

	if out, err := runGit(t, repo, "fetch", "-q", "origin"); err != nil {
		t.Fatalf("fetch after preflight: %v\n%s", err, out)
	}
	if out, err := runGit(t, repo, "pull", "-q", "origin"); err != nil {
		t.Fatalf("pull after preflight: %v\n%s", err, out)
	}
	fetch := mustGit(t, repo, "config", "--get-all", "remote.origin.fetch")
	if strings.Contains(fetch, brokenRefspec) {
		t.Fatalf("broken refspec still present: %q", fetch)
	}
	if !strings.Contains(fetch, notes.FetchRefspec) {
		t.Fatalf("transport should stay configured: %q", fetch)
	}
}

// Preflight only exists for the transport subcommands; anything else must
// leave git config exactly as it found it.
func TestPreflightIgnoresOtherSubcommands(t *testing.T) {
	repo := brokenClone(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	chdir(t, repo)

	preflight(realGit, "status", nil)
	preflight(realGit, "commit", []string{"-m", "x"})

	if !strings.Contains(mustGit(t, repo, "config", "--get-all", "remote.origin.fetch"), brokenRefspec) {
		t.Fatal("preflight must not touch config for non-transport subcommands")
	}
}
