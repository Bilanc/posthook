// Command posthook is the entrypoint binary. It dual-functions:
//
//  1. When invoked under the name "git" (via a symlink at ~/.local/bin/git),
//     it acts as a transparent git proxy that intercepts `commit` and `clone`
//     for capture. See internal/proxy.
//  2. Otherwise it runs the posthook CLI built on cobra.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bilanc/posthook/internal/commands"
	"github.com/bilanc/posthook/internal/proxy"
)

func main() {
	// argv[0] dispatch. os.Args[0] preserves the name the binary was invoked
	// as, even through a symlink — matching what Bun's process.argv0 did in
	// the TS version. We branch on the basename so /usr/local/bin/git and
	// ~/.local/bin/git both route to the proxy.
	invokedAs := filepath.Base(os.Args[0])
	if invokedAs == "git" {
		proxy.Run(os.Args[1:])
		return // unreachable; proxy.Run exits internally
	}

	root := commands.NewRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
