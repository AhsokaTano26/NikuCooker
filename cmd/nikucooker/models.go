package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/AhsokaTano26/NikuCooker/internal/app"
)

func newModelsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Inspect and download speech recognition models",
		Long: `Recognition models are downloaded on demand.

NikuCooker does not bundle them: the smallest is 75 MB and the largest is over a
gigabyte, and a user who only ever transcribes Japanese should not download
weights for ninety languages.`,
		Args: cobra.NoArgs,
	}

	cmd.AddCommand(
		newModelsListCmd(g),
		newModelsPullCmd(g),
		newModelsRemoveCmd(g),
	)
	return cmd
}

func newModelsListCmd(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the models available and which are installed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			application, err := g.openApp(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			records, err := application.Models.List(cmd.Context())
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(records)
			}

			if len(records) == 0 {
				fmt.Fprintln(out, "The model catalog is empty.")
				return nil
			}

			t := newTable("NAME", "STATUS", "SIZE", "NOTE")
			for _, record := range records {
				size := formatBytes(record.SizeBytes)
				if record.SizeBytes == 0 && record.ApproxBytes > 0 {
					// Marked as an estimate rather than shown as a fact: the
					// download size is the catalog's guess, and a user deciding
					// whether to spend a gigabyte deserves to know that.
					size = "~" + formatBytes(record.ApproxBytes)
				}
				t.add(record.Name, string(record.Status), size, record.Note)
			}
			return t.render(out)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newModelsPullCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pull <name>",
		Short: "Download a model",
		Long: `Downloads a model and verifies it.

The download is resumable and its progress is reported as it goes, because the
largest models take long enough that a silent wait is indistinguishable from a
hang.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			application, err := g.openApp(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			id, err := resolveModelID(cmd.Context(), application, args[0])
			if err != nil {
				return err
			}

			started := newProgressReporter(cmd, args[0])
			defer started.finish()

			if err := application.Models.Download(cmd.Context(), id, started.report); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "\nDownloaded %s\n", args[0])
			return nil
		},
	}
	return cmd
}

func newModelsRemoveCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Delete a downloaded model",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			application, err := g.openApp(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			id, err := resolveModelID(cmd.Context(), application, args[0])
			if err != nil {
				return err
			}

			if err := application.Models.Delete(cmd.Context(), id, false); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s\n", args[0])
			return nil
		},
	}
	return cmd
}

// resolveModelID accepts a bare name ("large-v3") or a full id ("asr:large-v3").
func resolveModelID(ctx context.Context, application *app.App, name string) (string, error) {
	records, err := application.Models.List(ctx)
	if err != nil {
		return "", err
	}

	for _, record := range records {
		if record.ID == name || record.Name == name {
			return record.ID, nil
		}
	}

	names := make([]string, 0, len(records))
	for _, record := range records {
		names = append(names, record.Name)
	}
	return "", fmt.Errorf("no model named %q; available: %s", name, joinNames(names))
}

func joinNames(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	out := ""
	for i, name := range names {
		if i > 0 {
			out += ", "
		}
		out += name
	}
	return out
}

// formatBytes renders a byte count for a human.
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}

	value := float64(bytes)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value/unit)
}
