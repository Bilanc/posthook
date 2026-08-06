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
