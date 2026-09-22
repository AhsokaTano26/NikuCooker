// Package qc runs the quality rules over the translated lines.
package qc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/AhsokaTano26/NikuCooker/internal/config"
	"github.com/AhsokaTano26/NikuCooker/internal/glossary"
	"github.com/AhsokaTano26/NikuCooker/internal/qc"
	"github.com/AhsokaTano26/NikuCooker/internal/stage"
	"github.com/AhsokaTano26/NikuCooker/internal/subtitle"
)

// PayloadName is the file this stage writes.
const PayloadName = "qc.json"

// Stage checks the translated lines.
type Stage struct{}

// New builds the stage.
func New() *Stage { return &Stage{} }

// Spec describes the stage.
func (s *Stage) Spec() stage.Spec {
	return stage.Spec{
		Name:    "qc",
		Version: "1",
		// Optional: a user who does not want the report can turn it off, and
		// nothing downstream consumes its output.
		Depends: []string{"translation"},
		// Checked against the polished lines when the polish pass ran, because
		// those are the lines that will be shipped. Auditing the intermediate
		// output would report on a file nobody will ever see.
		OptionalDepends: []string{"polish"},
		Optional:        true,
		ConfigKey:       "qc",
	}
}

// ConfigSubtree returns the thresholds the rules check against.
func (s *Stage) ConfigSubtree(cfg *config.Config) any { return cfg.QC }

// Fingerprint records the project's current lines.
//
// The glossary is read but kept out of the key: a glossary edit changes what is
// reported, not what is stored, and the findings are recomputed every run
// anyway because they are replaced rather than accumulated. The lines are in the
// key because an edit changes which line each finding belongs to.
func (s *Stage) Fingerprint(ctx context.Context, env *stage.Env) (map[string]string, error) {
	if env.Services.Lines == nil {
		return nil, nil
	}

	digest, err := env.Services.Lines.LinesHash(ctx, env.ProjectID)
	if err != nil {
		return nil, err
	}
	if digest == "" {
		return nil, nil
	}
	return map[string]string{"lines": digest}, nil
}

// Run checks the lines.
func (s *Stage) Run(ctx context.Context, env *stage.Env) (*stage.Result, error) {
	if !env.Config.QC.Enabled {
		// An explicit no-op rather than a skip. The stage ran and found nothing
		// to say, which is a different statement from the stage being disabled,
		// and the artifact records which of the two happened.
		env.Log.Info("quality checking is disabled in the configuration")
		return s.writeReport(env, &qc.Report{
			Findings: []qc.Finding{},
			Counts:   map[qc.Severity]int{},
		})
	}

	set, err := s.lines(ctx, env)
	if err != nil {
		return nil, err
	}

	entries, err := s.glossary(ctx, env)
	if err != nil {
		return nil, err
	}

	cfg := env.Config.QC
	report := qc.Check(set, qc.Options{
		MaxCPS:          cfg.MaxCPS,
		MaxDuration:     cfg.MaxDuration,
		MinDuration:     cfg.MinDuration,
		MaxChars:        cfg.MaxChars,
		DuplicateWindow: cfg.DuplicateWindow,
		Glossary:        entries,
	})

	env.Log.Info("checked the lines", "result", report.Summary())

	if report.HasErrors() {
		// Not a stage failure. The render is still worth producing — a file
		// with two flagged lines is more useful than no file — and the findings
		// are what tells the user to look at it.
		env.Log.Warn("some lines have errors that a render will carry through",
			"errors", report.Counts[qc.SeverityError])
	}

	return s.writeReport(env, report)
}

// writeReport writes the findings artifact.
func (s *Stage) writeReport(env *stage.Env, report *qc.Report) (*stage.Result, error) {
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("qc: encode the report: %w", err)
	}

	final := filepath.Join(env.OutDir, PayloadName)
	if err := os.WriteFile(final+".tmp", encoded, 0o644); err != nil {
		return nil, fmt.Errorf("qc: write the report: %w", err)
	}
	if err := os.Rename(final+".tmp", final); err != nil {
		return nil, fmt.Errorf("qc: write the report: %w", err)
	}

	env.ReportProgress(1, "done")

	return &stage.Result{
		Primary: PayloadName,
		Metadata: map[string]any{
			"checked_segments": report.CheckedSegments,
			"errors":           report.Counts[qc.SeverityError],
			"warnings":         report.Counts[qc.SeverityWarning],
			"notes":            report.Counts[qc.SeverityInfo],
			"codes":            countByCode(report),
		},
	}, nil
}

// lines returns the lines to check, preferring the polished set when the polish
// stage ran.
func (s *Stage) lines(ctx context.Context, env *stage.Env) (*subtitle.Set, error) {
	// The stored lines first, for the same reason the subtitle stage reads
	// them: they are what the user believes the project says, and auditing the
	// intermediate artifact would report on a file nobody will see.
	if env.Services.Lines != nil {
		current, err := env.Services.Lines.CurrentLines(ctx, env.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("qc: %w", err)
		}
		if current != nil && len(current.Segments) > 0 {
			return current, nil
		}
	}

	var polished subtitle.Set
	ok, err := env.ReadOptional("polish", &polished)
	if err != nil {
		return nil, fmt.Errorf("qc: %w", err)
	}
	if ok && len(polished.Segments) > 0 {
		return &polished, nil
	}

	var set subtitle.Set
	if err := env.ReadInput("translation", &set); err != nil {
		return nil, fmt.Errorf("qc: %w", err)
	}
	return &set, nil
}

// glossary loads the terminology the glossary rule checks against.
func (s *Stage) glossary(ctx context.Context, env *stage.Env) ([]glossary.Entry, error) {
	if env.Services.Glossary == nil {
		return nil, nil
	}
	entries, err := env.Services.Glossary.List(ctx, env.ProjectID, false)
	if err != nil {
		return nil, fmt.Errorf("qc: %w", err)
	}
	return entries, nil
}

// countByCode reports how often each rule fired, which is what a user looks at
// to decide whether a problem is systemic or a one-off.
func countByCode(report *qc.Report) map[string]int {
	counts := map[string]int{}
	for _, finding := range report.Findings {
		counts[finding.Code]++
	}
	return counts
}
