package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/logx"
	"github.com/bilanc/posthook/internal/notes"
	"github.com/bilanc/posthook/internal/paths"

	"github.com/spf13/cobra"
)

func newNotesCmd() *cobra.Command {
	var remote string
	cmd := &cobra.Command{
		Use:   "notes",
		Short: "Move attribution notes (refs/notes/posthook) to and from a remote",
		Long: `Attribution travels with the repo in refs/notes/posthook. With the git shadow
installed this happens by itself: every successful push also pushes the notes,
and every fetch/pull/clone brings teammates' notes down. These commands do the
same thing by hand — for the fallback (no-shadow) install, for CI, or to check
what is there.

  posthook notes push   [repo]   merge the remote's notes into ours, then push
  posthook notes fetch  [repo]   fetch the remote's notes and merge them locally
  posthook notes status [repo]   how many commits carry notes, locally and on the remote
  posthook notes configure [repo]  add the fetch refspec so plain git fetch carries notes

Set POSTHOOK_NOTES_SYNC=0 to turn automatic transport off entirely.`,
	}
	cmd.PersistentFlags().StringVar(&remote, "remote", "origin", "Remote to talk to")

	cmd.AddCommand(
		&cobra.Command{
			Use:   "push [repo]",
			Short: "Merge the remote's notes into ours, then push refs/notes/posthook",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				root, err := notesRepoRoot(args)
				if err != nil {
					return err
				}
				if !notes.RefExists(root, paths.NotesRef) {
					logx.Info("no local attribution notes yet — commit something an agent edited first")
					return nil
				}
				notes.EnsureRefspec(root, remote)
				if err := notes.Push(root, remote); err != nil {
					return err
				}
				logx.Infof("pushed %s to %s", paths.NotesRef, remote)
				return nil
			},
		},
		&cobra.Command{
			Use:   "fetch [repo]",
			Short: "Fetch the remote's notes and merge them into refs/notes/posthook",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				root, err := notesRepoRoot(args)
				if err != nil {
					return err
				}
				notes.EnsureRefspec(root, remote)
				if err := notes.Fetch(root, remote); err != nil {
					return fmt.Errorf("%s has no attribution notes yet (or is unreachable): %w", remote, err)
				}
				logx.Infof("fetched %s from %s", paths.NotesRef, remote)
				return nil
			},
		},
		&cobra.Command{
			Use:   "configure [repo]",
			Short: "Add the notes fetch refspec so ordinary git fetch/pull carries notes",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				root, err := notesRepoRoot(args)
				if err != nil {
					return err
				}
				if notes.EnsureRefspec(root, remote) {
					logx.Infof("added %s to remote.%s.fetch", notes.FetchRefspec, remote)
				} else {
					logx.Info("notes fetch refspec already configured")
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "status [repo]",
			Short: "Show how many commits carry attribution notes, locally and on the remote",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(_ *cobra.Command, args []string) error {
				root, err := notesRepoRoot(args)
				if err != nil {
					return err
				}
				local := countLines(gitx.Run(root, "notes", "--ref="+paths.NotesRef, "list"))
				tracking := countLines(gitx.Run(root, "notes", "--ref="+notes.TrackingRef, "list"))
				remoteSha := gitx.Run(root, "ls-remote", "--quiet", remote, paths.NotesRef)
				fmt.Printf("posthook notes — %s\n\n", root)
				fmt.Printf("  local   %-32s %d commit(s) annotated\n", paths.NotesRef, local)
				fmt.Printf("  fetched %-32s %d commit(s) annotated (last fetched from %s)\n", notes.TrackingRef, tracking, remote)
				if remoteSha == "" {
					fmt.Printf("  remote  %-32s absent on %s — run `posthook notes push`\n", paths.NotesRef, remote)
				} else {
					fmt.Printf("  remote  %-32s present on %s (%s)\n", paths.NotesRef, remote, remoteSha[:7])
				}
				if notes.Enabled() {
					fmt.Println("\n  automatic transport: on (git shadow pushes on push, merges on fetch/pull/clone)")
				} else {
					fmt.Println("\n  automatic transport: off (POSTHOOK_NOTES_SYNC=0)")
				}
				return nil
			},
		},
	)
	return cmd
}

func notesRepoRoot(args []string) (string, error) {
	start := "."
	if len(args) == 1 {
		start = args[0]
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("no such path: %s", abs)
	}
	root := gitx.FindRepoRoot(abs)
	if root == "" {
		return "", errors.New("not inside a git repo: " + abs)
	}
	return root, nil
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := 1
	for _, r := range s {
		if r == '\n' {
			n++
		}
	}
	return n
}
