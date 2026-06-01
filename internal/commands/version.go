package commands

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Build metadata. The defaults keep a plain `go build` honest about being an
// unversioned dev binary; release builds overwrite them via -ldflags -X (see
// .goreleaser.yaml). The vars are unexported — `go tool link -X` sets package
// vars regardless of export, so the goreleaser ldflags target these directly.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Version returns the build version string, for callers (e.g. the sync
// User-Agent) that want to stamp it onto outbound requests.
func Version() string { return version }

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the posthook version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("posthook %s (commit %s, built %s, %s/%s, %s)\n",
				version, commit, date, runtime.GOOS, runtime.GOARCH, runtime.Version())
		},
	}
}
