package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AhsokaTano26/NikuCooker/internal/app"
	"github.com/AhsokaTano26/NikuCooker/internal/pipeline"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
	"github.com/AhsokaTano26/NikuCooker/internal/qc"
)

func newRunCmd(g *globals) *cobra.Command {
	var (
		force     bool
		fromStage string
		toStage   string
		only      []string
		quiet     bool
		asJSON    bool
	)

	cmd := &cobra.Command{
		Use:   "run <project>",
		Short: "Run the pipeline for a project",
		Long: `Runs every stage that does not already have a cached artifact.

Stages are skipped when their inputs and configuration are unchanged, so
re-running after an edit recomputes only what the edit affected — and a
translation whose lines have not changed costs nothing, because the line cache is
separate from the stage cache and survives it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The run command has its own progress display and summary, which
			// say the same things as the core's info logs in a form meant for a
			// person. Leaving both on doubles every line and buries the
			// summary. Raising the level is what brings the logs back.
			if !cmd.Flags().Changed("log-level") {
				g.logLevel = "warn"
			}

			application, err := g.openApp(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			id, err := resolveProjectID(cmd, application, args[0])
			if err != nil {
				return err
			}

			var observer pipeline.Observer = pipeline.NopObserver{}
			if !quiet {
				observer = newProgressObserver(cmd)
			}

			result, runErr := application.Run(cmd.Context(), app.RunOptions{
				ProjectID: id,
				Force:     force,
				Only:      only,
				FromStage: fromStage,
				ToStage:   toStage,
				Observer:  observer,
			})
			if result == nil || result.Plan == nil {
				return runErr
			}

			// A stage failure is reported on the plan rather than returned, so
			// the summary is printed either way. A run whose render failed
			// still produced subtitles, and hiding them behind an error would
			// throw away work the user paid for.
			summary, failed := summarise(application, result)
			if asJSON {
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				if err := encoder.Encode(summary); err != nil {
					return err
				}
			} else {
				printSummary(cmd, summary)
			}

			if runErr != nil {
				return runErr
			}
			if failed {
				return fmt.Errorf("one or more stages did not complete")
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "ignore the artifact cache and recompute")
	cmd.Flags().StringVar(&fromStage, "from", "", "start at this stage")
	cmd.Flags().StringVar(&toStage, "to", "", "stop after this stage")
	cmd.Flags().StringSliceVar(&only, "only", nil, "run only these stages (comma separated)")
	cmd.Flags().BoolVar(&quiet, "quiet", false, "suppress progress output")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the summary as JSON")

	return cmd
}

// stageSummary is one stage's outcome.
type stageSummary struct {
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Reason   string  `json:"reason,omitempty"`
	Duration float64 `json:"duration_s"`
	Error    string  `json:"error,omitempty"`

	// Metadata is the stage's own record of what it produced: line counts,
	// token usage, which encoder ran.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// runSummary is the whole run's outcome.
type runSummary struct {
	JobID  string         `json:"job_id"`
	Stages []stageSummary `json:"stages"`

	// QC is the quality report when the qc stage ran, and nil otherwise.
	QC *qc.Report `json:"qc,omitempty"`

	// Artifacts are the cache entries this run produced or reused, one per
	// stage, as absolute paths.
	//
	// Reported under --json and not printed: they are cache internals, and a
	// list that includes audio.wav and probe.json alongside the subtitles buries
	// the two files a person actually wanted.
	Artifacts []string `json:"artifacts,omitempty"`

	// OutputDir is where the finished files were published.
	OutputDir string        `json:"output_dir,omitempty"`
	Outputs   []outputEntry `json:"outputs,omitempty"`
}

// outputEntry is one published file.
type outputEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// summarise renders a plan as something printable.
func summarise(application *app.App, result *app.RunResult) (*runSummary, bool) {
	summary := &runSummary{JobID: result.JobID, Stages: []stageSummary{}}
	failed := false

	for _, sp := range result.Plan.Stages {
		entry := stageSummary{
			Name:     sp.Stage.Spec().Name,
			State:    string(sp.State),
			Reason:   sp.Reason,
			Duration: sp.Duration.Seconds(),
		}
		switch {
		case sp.Err != nil:
			entry.Error = sp.Err.Error()
			failed = true
		case sp.Blocked:
			// Not an error the pipeline raised, but the run did not do what it
			// was asked: a stage that was selected could not run. Reporting
			// success here would let a script carry on as though it had.
			entry.Error = sp.Reason
			failed = true
		}
		if sp.Result != nil {
			entry.Metadata = sp.Result.Metadata
		}
		summary.Stages = append(summary.Stages, entry)

		// The QC report is surfaced rather than left in an artifact: it is the
		// one output whose whole purpose is to be read.
		if sp.Stage.Spec().Name == "qc" && sp.State == pipeline.StateCompleted && sp.Artifact != nil {
			var report qc.Report
			if err := application.Artifacts.For(sp.Artifact.ProjectID).
				Decode(sp.Artifact, &report); err == nil {
				summary.QC = &report
			}
		}

		if sp.Artifact != nil {
			if path, err := application.Artifacts.For(sp.Artifact.ProjectID).Path(sp.Artifact); err == nil {
				summary.Artifacts = append(summary.Artifacts, path)
			}
		}
	}

	sort.Strings(summary.Artifacts)

	// What a run exists to produce, published at the end of it. Read back from
	// disk rather than reconstructed from the plan, so the listing is what is
	// actually there.
	if files, err := application.OutputFiles(result.ProjectID); err == nil && len(files) > 0 {
		summary.OutputDir = project.OutputDir(application.DataDir(), result.ProjectID)
		for _, file := range files {
			summary.Outputs = append(summary.Outputs, outputEntry{
				Name:      file.Name,
				Path:      filepath.Join(summary.OutputDir, file.Name),
				SizeBytes: file.SizeBytes,
			})
		}
	}

	return summary, failed
}

// printSummary writes the human-readable report.
func printSummary(cmd *cobra.Command, summary *runSummary) {
	out := cmd.OutOrStdout()

	fmt.Fprintln(out)
	width := 0
	for _, stage := range summary.Stages {
		if len(stage.Name) > width {
			width = len(stage.Name)
		}
	}

	for _, stage := range summary.Stages {
		marker := stateMarker(stage.State)

		line := fmt.Sprintf("  %s %-*s", marker, width, stage.Name)
		if detail := stageDetail(stage); detail != "" {
			line += "  " + detail
		}
		fmt.Fprintln(out, line)

		if stage.Error != "" {
			fmt.Fprintf(out, "    %-*s  %s\n", width, "", firstLine(stage.Error))
		}
	}

	if summary.QC != nil {
		fmt.Fprintf(out, "\n  quality: %s\n", summary.QC.Summary())
		for _, finding := range summary.QC.BySeverity(qc.SeverityWarning) {
			location := "project"
			if finding.SegmentID != "" {
				location = finding.SegmentID
			}
			fmt.Fprintf(out, "    %-9s %-10s %s\n", location, finding.Code, finding.Message)
		}
	}

	if len(summary.Outputs) > 0 {
		fmt.Fprintf(out, "\n  output:\n    %s\n\n", summary.OutputDir)

		// Padded to the longest name rather than to a fixed column, and by
		// display width rather than by byte count — a filename is free to be
		// longer than any column and to be full of characters that are two
		// terminals wide.
		width := 0
		for _, file := range summary.Outputs {
			if w := displayWidth(file.Name); w > width {
				width = w
			}
		}
		for _, file := range summary.Outputs {
			fmt.Fprintf(out, "      %s  %s\n", pad(file.Name, width), formatBytes(file.SizeBytes))
		}
	}
}

// stageDetail renders a stage's one-line outcome.
func stageDetail(stage stageSummary) string {
	parts := []string{stage.State}

	if stage.Reason != "" && stage.State == "skipped" {
		parts = append(parts, "("+stage.Reason+")")
	}
	if stage.Duration > 0 {
		parts = append(parts, fmt.Sprintf("%.1fs", stage.Duration))
	}
	if detail := metadataDetail(stage.Metadata); detail != "" {
		parts = append(parts, detail)
	}
	return strings.Join(parts, "  ")
}

// metadataDetail picks the few metadata keys worth showing.
//
// A fixed list rather than everything: a stage reports a dozen diagnostic keys
// and printing them all turns a summary into a wall of numbers nobody reads.
func metadataDetail(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}

	var parts []string
	add := func(key, format string) {
		value, ok := metadata[key]
		if !ok {
			return
		}
		if number, ok := value.(float64); ok {
			parts = append(parts, fmt.Sprintf(format, number))
			return
		}
		if text, ok := value.(string); ok && text != "" {
			parts = append(parts, text)
		}
	}

	add("line_count", "%d lines")
	add("translated", "%d translated")
	add("from_cache", "%d cached")
	add("failed", "%d failed")
	add("mode", "%s")
	add("encoder", "%s")
	add("output_bytes", "%d bytes")

	return strings.Join(parts, ", ")
}

// stateMarker renders a stage state as a single character.
func stateMarker(state string) string {
	switch state {
	case "completed":
		return "✓"
	case "cached":
		return "="
	case "skipped":
		return "-"
	case "failed":
		return "✗"
	case "cancelled":
		return "·"
	default:
		return " "
	}
}
