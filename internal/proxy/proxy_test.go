package proxy

import (
	"reflect"
	"testing"
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
