package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/AhsokaTano26/NikuCooker/internal/app"
	"github.com/AhsokaTano26/NikuCooker/internal/project"
)

func newProjectCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Create and inspect projects",
		Long: "A project is one video and its translation, stored as a self-contained\n" +
			"directory that can be copied, moved or backed up without breaking.",
		Args: cobra.NoArgs,
	}

	cmd.AddCommand(
		newProjectCreateCmd(g),
		newProjectListCmd(g),
		newProjectShowCmd(g),
		newProjectDeleteCmd(g),
	)
	return cmd
}

func newProjectCreateCmd(g *globals) *cobra.Command {
	var (
		name           string
		source         string
		sourceLanguage string
		targetLanguage string
		style          string
		asJSON         bool
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a project from a video file",
		Long: `Copies the source media into a new project and records it.

The copy is deliberate. A project that referenced the file in place would break
the moment the user moved, renamed or deleted it, and the whole data tree is
meant to be relocatable.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			application, err := g.openApp(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			if source == "" {
				return fmt.Errorf("--source is required")
			}

			absolute, err := filepath.Abs(source)
			if err != nil {
				return err
			}
			info, err := os.Stat(absolute)
			if err != nil {
				return fmt.Errorf("cannot read the source media: %w", err)
			}
			if info.IsDir() {
				return fmt.Errorf("%s is a directory; point --source at a media file", absolute)
			}

			if name == "" {
				name = strings.TrimSuffix(filepath.Base(absolute), filepath.Ext(absolute))
			}

			created, err := application.Projects.Create(cmd.Context(), project.CreateRequest{
				Name:           name,
				SourceLanguage: sourceLanguage,
				TargetLanguage: targetLanguage,
				Style:          style,
				SourceName:     filepath.Base(absolute),
				SourceOrigin:   absolute,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(created)
			}

			fmt.Fprintf(out, "Created %s\n", created.Name)
			fmt.Fprintf(out, "  id:   %s\n", created.ID)
			fmt.Fprintf(out, "  dir:  %s\n", project.Dir(application.DataDir(), created.ID))
			fmt.Fprintf(out, "  pair: %s → %s, style %s\n",
				created.SourceLanguage, created.TargetLanguage, created.Style)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "project name; defaults to the source filename")
	cmd.Flags().StringVar(&source, "source", "", "path to the video file (required)")
	cmd.Flags().StringVar(&sourceLanguage, "source-language", "ja", "source language as a BCP-47 tag")
	cmd.Flags().StringVar(&targetLanguage, "target-language", "zh-Hans", "target language as a BCP-47 tag")
	cmd.Flags().StringVar(&style, "style", "fansub", "translation style: literal, natural or fansub")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the project as JSON")

	return cmd
}

func newProjectListCmd(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List projects",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			application, err := g.openApp(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			projects, total, err := application.Projects.List(cmd.Context(),
				project.ListQuery{Limit: 200})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(projects)
			}

			if total == 0 {
				fmt.Fprintln(out, "No projects yet. Create one with `nikucooker project create --source <video>`.")
				return nil
			}

			t := newTable("ID", "NAME", "PAIR", "STYLE", "STATUS")
			for _, p := range projects {
				t.add(shortID(p.ID), p.Name,
					p.SourceLanguage+" → "+p.TargetLanguage, p.Style, string(p.Status))
			}
			return t.render(out)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newProjectShowCmd(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one project in detail",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			application, err := g.openApp(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			id, err := resolveProjectID(cmd, application, args[0])
			if err != nil {
				return err
			}

			p, err := application.Projects.Get(cmd.Context(), id)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(p)
			}

			fmt.Fprintf(out, "%s\n", p.Name)
			fmt.Fprintf(out, "  id:       %s\n", p.ID)
			fmt.Fprintf(out, "  pair:     %s → %s, style %s\n", p.SourceLanguage, p.TargetLanguage, p.Style)
			fmt.Fprintf(out, "  status:   %s\n", p.Status)
			fmt.Fprintf(out, "  source:   %s\n", application.Projects.SourceAbs(p))
			fmt.Fprintf(out, "  created:  %s\n", p.CreatedAt.Format("2006-01-02 15:04"))

			// The most recent run's state, which is what a user actually wants
			// to know after `show`: did it work, and where did it get to.
			jobs, _, err := application.Jobs.ListByProject(cmd.Context(), p.ID, 3, 0)
			if err == nil && len(jobs) > 0 {
				fmt.Fprintln(out, "  recent runs:")
				for _, job := range jobs {
					line := fmt.Sprintf("    %s  %-10s %3.0f%%",
						job.CreatedAt.Format("01-02 15:04"), job.Status, job.Progress*100)
					if job.ErrorMessage != "" {
						line += "  " + firstLine(job.ErrorMessage)
					}
					fmt.Fprintln(out, line)
				}
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newProjectDeleteCmd(g *globals) *cobra.Command {
	var (
		force     bool
		keepFiles bool
	)

	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a project",
		Long: `Removes a project from the database.

The files are kept unless --delete-files is given. A project directory holds the
source media and every artifact produced from it, which for a feature-length
video is hours of work — deleting it by default would make a mistaken command
unrecoverable.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			application, err := g.openApp(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			id, err := resolveProjectID(cmd, application, args[0])
			if err != nil {
				return err
			}

			p, err := application.Projects.Get(cmd.Context(), id)
			if err != nil {
				return err
			}

			if !force {
				fmt.Fprintf(cmd.OutOrStdout(),
					"Delete project %q? Its files will be kept. Pass --force to skip this prompt.\n", p.Name)
				return fmt.Errorf("refusing to delete without --force")
			}

			if err := application.Projects.Delete(cmd.Context(), id, !keepFiles); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if keepFiles {
				fmt.Fprintf(out, "Deleted %s. Its directory is at %s\n",
					p.Name, project.Dir(application.DataDir(), id))
				return nil
			}
			fmt.Fprintf(out, "Deleted %s and its files.\n", p.Name)
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "delete without asking")
	cmd.Flags().BoolVar(&keepFiles, "keep-files", false, "keep the project directory")
	return cmd
}

// resolveProjectID accepts either a full id or an unambiguous prefix.
//
// Ids are UUIDs and nobody types them in full. A prefix that matches more than
// one project is an error rather than a guess, because acting on the wrong
// project is not a mistake that undoes cleanly.
func resolveProjectID(cmd *cobra.Command, application *app.App, prefix string) (string, error) {
	if prefix == "" {
		return "", fmt.Errorf("no project was named")
	}

	projects, _, err := application.Projects.List(cmd.Context(), project.ListQuery{Limit: 1000})
	if err != nil {
		return "", err
	}

	for _, p := range projects {
		if p.ID == prefix {
			return p.ID, nil
		}
	}

	var matches []string
	for _, p := range projects {
		if strings.HasPrefix(p.ID, prefix) {
			matches = append(matches, p.ID)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no project matches %q", prefix)
	case 1:
		return matches[0], nil
	default:
		short := make([]string, 0, len(matches))
		for _, id := range matches {
			short = append(short, shortID(id))
		}
		return "", fmt.Errorf("%q matches %d projects: %s",
			prefix, len(matches), strings.Join(short, ", "))
	}
}

// shortID renders the leading segment of a UUID, which is what a person uses.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
