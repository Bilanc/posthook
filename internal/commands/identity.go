package commands

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/bilanc/posthook/internal/config"
	"github.com/bilanc/posthook/internal/gitx"
	"github.com/bilanc/posthook/internal/paths"
	"github.com/bilanc/posthook/internal/store"

	"github.com/spf13/cobra"
)

func newIdentityCmd() *cobra.Command {
	var (
		setup    bool
		setEmail string
		setName  string
	)
	cmd := &cobra.Command{
		Use:   "identity",
		Short: "Show or set the engineer identity stamped on sessions",
		Long: `The engineer identity (work email + name) is stamped on every session and is
how the cloud attributes activity to a person. The confirmed identity in
~/.posthook/config.json takes precedence over per-repo git config, which is
often unset or a personal address.

  posthook identity                 show the resolved identity and its source
  posthook identity --setup         interactive setup: detect, confirm, persist
                                    (run by the team installer; idempotent)
  posthook identity --set-email E   write the identity non-interactively
       [--set-name N]

Environment:
  POSTHOOK_ENGINEER_EMAIL  identity override; --setup persists it without prompting
  POSTHOOK_ENGINEER_NAME   name override
`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if setEmail != "" || setName != "" {
				return runIdentitySet(setEmail, setName)
			}
			if setup {
				return runIdentitySetup()
			}
			return runIdentityShow()
		},
	}
	cmd.Flags().BoolVar(&setup, "setup", false, "Interactive setup: detect candidates, confirm via the terminal, persist")
	cmd.Flags().StringVar(&setEmail, "set-email", "", "Write the engineer email to ~/.posthook/config.json")
	cmd.Flags().StringVar(&setName, "set-name", "", "Write the engineer name to ~/.posthook/config.json")
	return cmd
}

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func looksLikeEmail(s string) bool { return emailRe.MatchString(s) }

func runIdentityShow() error {
	disk, err := readDiskConfig()
	if err != nil {
		return err
	}
	email, name := disk.Engineer.Email, disk.Engineer.Name
	source := "config (" + config.Path() + ")"
	if v := os.Getenv("POSTHOOK_ENGINEER_EMAIL"); v != "" {
		email, source = v, "env (POSTHOOK_ENGINEER_EMAIL)"
	}
	if v := os.Getenv("POSTHOOK_ENGINEER_NAME"); v != "" {
		name = v
	}
	if email == "" {
		gitEmail, gitName := globalGitIdentity()
		if gitEmail != "" {
			email, source = gitEmail, "git config (fallback — not confirmed)"
			if name == "" {
				name = gitName
			}
		}
	}

	fmt.Println("posthook identity — who sessions are attributed to:")
	if email == "" {
		fmt.Println("  email:  — (not set)")
		fmt.Println()
		fmt.Println("  Sessions are syncing without attribution. Set it with:")
		fmt.Println("    posthook identity --setup")
		return nil
	}
	fmt.Printf("  email:  %s\n", email)
	fmt.Printf("  name:   %s\n", orDash(name))
	if disk.Engineer.GitHubLogin != "" {
		fmt.Printf("  github: %s\n", disk.Engineer.GitHubLogin)
	}
	fmt.Printf("  source: %s\n", source)
	return nil
}

func runIdentitySet(email, name string) error {
	if email != "" && !looksLikeEmail(email) {
		return fmt.Errorf("%q does not look like an email address", email)
	}
	disk, err := readDiskConfig()
	if err != nil {
		return err
	}
	if email != "" {
		disk.Engineer.Email = email
	}
	if name != "" {
		disk.Engineer.Name = name
	}
	if err := config.Save(disk); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", config.Path())
	fmt.Printf("  email: %s\n", orDash(disk.Engineer.Email))
	fmt.Printf("  name:  %s\n", orDash(disk.Engineer.Name))
	if disk.Engineer.Email != "" {
		stampUnattributedSessions(disk.Engineer.Email, disk.Engineer.Name)
	}
	return nil
}

// runIdentitySetup is the installer entrypoint. It must never fail a
// `curl | sh` install: no TTY and no env override just means "set it later",
// reported as a warning with exit 0.
func runIdentitySetup() error {
	disk, err := readDiskConfig()
	if err != nil {
		return err
	}
	if disk.Engineer.Email != "" {
		fmt.Printf("identity already set: %s — keeping it (change with: posthook identity --set-email)\n", disk.Engineer.Email)
		return nil
	}

	// Env-provided identity (MDM / dotfiles installs): persist without prompting.
	if v := os.Getenv("POSTHOOK_ENGINEER_EMAIL"); v != "" {
		if !looksLikeEmail(v) {
			return fmt.Errorf("POSTHOOK_ENGINEER_EMAIL=%q does not look like an email address", v)
		}
		name := os.Getenv("POSTHOOK_ENGINEER_NAME")
		if name == "" {
			_, name = globalGitIdentity()
		}
		return persistIdentity(disk, v, name)
	}

	detEmail, detName := globalGitIdentity()

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// Non-interactive and no env override. Don't persist the unconfirmed git
		// email — sessions still fall back to it at ingest time — just say how
		// to finish setup.
		fmt.Fprintln(os.Stderr, "[posthook] warning: no terminal available — engineer identity not set.")
		fmt.Fprintln(os.Stderr, "  Set it with: posthook identity --setup   (or --set-email you@company.com)")
		return nil
	}
	defer tty.Close()

	email, name := promptIdentity(tty, detEmail, detName)
	if email == "" {
		fmt.Fprintln(os.Stderr, "[posthook] warning: no valid email entered — engineer identity not set.")
		fmt.Fprintln(os.Stderr, "  Set it later with: posthook identity --setup")
		return nil
	}
	return persistIdentity(disk, email, name)
}

// promptIdentity runs the confirm-or-type flow on the controlling terminal.
// Returns ("", "") if the user can't produce a valid email in a few tries.
func promptIdentity(tty *os.File, detEmail, detName string) (string, string) {
	r := bufio.NewReader(tty)

	email := ""
	for attempt := 0; attempt < 3 && email == ""; attempt++ {
		candidate := ""
		if detEmail != "" && attempt == 0 {
			who := detEmail
			if detName != "" {
				who += " (" + detName + ")"
			}
			fmt.Fprintf(tty, "Detected git identity: %s\n", who)
			fmt.Fprintf(tty, "Use this as your work email? [Y/n, or type a different email]: ")
			line := readLine(r)
			switch {
			case line == "" || strings.EqualFold(line, "y") || strings.EqualFold(line, "yes"):
				candidate = detEmail
			case strings.Contains(line, "@"):
				candidate = line
			default:
				fmt.Fprintf(tty, "Work email: ")
				candidate = readLine(r)
			}
		} else {
			fmt.Fprintf(tty, "Work email: ")
			candidate = readLine(r)
		}

		if !looksLikeEmail(candidate) {
			fmt.Fprintf(tty, "  %q doesn't look like an email address — try again.\n", candidate)
			continue
		}
		email = candidate
	}
	if email == "" {
		return "", ""
	}

	name := detName
	if name != "" {
		fmt.Fprintf(tty, "Your name [%s]: ", name)
	} else {
		fmt.Fprintf(tty, "Your name: ")
	}
	if line := readLine(r); line != "" {
		name = line
	}
	return email, name
}

func persistIdentity(disk config.Config, email, name string) error {
	disk.Engineer.Email = email
	disk.Engineer.Name = name
	if login := detectGitHubLogin(); login != "" {
		disk.Engineer.GitHubLogin = login
	}
	if err := config.Save(disk); err != nil {
		return err
	}
	who := email
	if name != "" {
		who = name + " <" + email + ">"
	}
	fmt.Printf("identity set: %s (wrote %s)\n", who, config.Path())
	stampUnattributedSessions(email, name)
	return nil
}

// stampUnattributedSessions retro-stamps local sessions that have no engineer
// yet. The local store is single-user — every session on this machine belongs
// to this engineer — so this is safe; sessions already attributed (e.g. from a
// repo-level git config) are left untouched. Best-effort: identity setup
// shouldn't fail because the DB is busy.
func stampUnattributedSessions(email, name string) {
	db, err := store.Open()
	if err != nil {
		return
	}
	res, err := db.Exec(`
		UPDATE sessions
		SET engineer_email = ?,
		    engineer_name  = COALESCE(engineer_name, NULLIF(?, '')),
		    synced_at      = NULL
		WHERE engineer_email IS NULL`, email, name)
	if err != nil {
		return
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		fmt.Printf("attributed %d existing session(s) to %s\n", n, email)
	}
}

// globalGitIdentity reads the user-level git identity (global/system config,
// not any repo's), which is the best unconfirmed default we have.
func globalGitIdentity() (email, name string) {
	home := paths.Home()
	return gitx.Run(home, "config", "--global", "--get", "user.email"),
		gitx.Run(home, "config", "--global", "--get", "user.name")
}

// detectGitHubLogin asks the gh CLI for the authenticated login. Purely
// best-effort enrichment — most machines won't have gh, and that's fine.
func detectGitHubLogin() string {
	gh, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, gh, "api", "user", "--jq", ".login").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func readLine(r *bufio.Reader) string {
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}
