package main

import (
	"encoding/json"
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// Injected at build time via -ldflags. See the Makefile and .goreleaser.yaml.
//
// codeRevision is not merely informational: it participates in every artifact
// cache key, which is what stops a rebuilt binary from serving output produced
// by an older implementation. See docs/artifact-cache.md §2.2.
var (
	version      = "dev"
	commit       = "none"
	codeRevision = "dev"
	buildDate    = "unknown"
)

type versionInfo struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	CodeRevision string `json:"code_revision"`
	BuildDate    string `json:"build_date"`
	GoVersion    string `json:"go_version"`
	Platform     string `json:"platform"`
}

func currentVersion() versionInfo {
	return versionInfo{
		Version:      version,
		Commit:       commit,
		CodeRevision: codeRevision,
		BuildDate:    buildDate,
		GoVersion:    runtime.Version(),
		Platform:     runtime.GOOS + "-" + runtime.GOARCH,
	}
}

func newVersionCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print version and build information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := currentVersion()
			out := cmd.OutOrStdout()

			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}

			fmt.Fprintf(out, "%s %s (%s)\n", appName, info.Version, info.Platform)
			fmt.Fprintf(out, "  commit:        %s\n", info.Commit)
			fmt.Fprintf(out, "  code revision: %s\n", info.CodeRevision)
			fmt.Fprintf(out, "  built:         %s\n", info.BuildDate)
			fmt.Fprintf(out, "  go:            %s\n", info.GoVersion)
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}
