package main

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/AhsokaTano26/NikuCooker/internal/app"
	"github.com/AhsokaTano26/NikuCooker/internal/glossary"
)

func newGlossaryCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "glossary",
		Short: "Manage the terminology a translation must respect",
		Long: `A glossary entry fixes how one term is translated, everywhere.

Characters' names, place names and technical terms are the reason this exists: a
model translating line by line has no way to know that the name it rendered one
way in episode one should not drift by episode three, and the drift is the most
visible way a fansub looks careless.

Entries without --project apply to every project. An entry that applies to one
project wins over a global one for the same source, because it is the more
specific statement of intent.`,
		Args: cobra.NoArgs,
	}

	cmd.AddCommand(
		newGlossaryAddCmd(g),
		newGlossaryListCmd(g),
		newGlossaryRemoveCmd(g),
	)
	return cmd
}

func newGlossaryAddCmd(g *globals) *cobra.Command {
	var (
		project  string
		termType string
		note     string
		priority int
		disabled bool
		asJSON   bool
	)

	cmd := &cobra.Command{
		Use:   "add <source> <target>",
		Short: "Add a glossary entry",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			application, err := g.openApp(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			entry := &glossary.Entry{
				Source:   args[0],
				Target:   args[1],
				Type:     glossary.Type(termType),
				Note:     note,
				Priority: priority,
				Enabled:  !disabled,
			}

			if project != "" {
				id, err := resolveProjectID(cmd, application, project)
				if err != nil {
					return err
				}
				entry.ProjectID = &id
			}

			if err := application.Glossary.Save(cmd.Context(), entry); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(entry)
			}

			scope := "global"
			if entry.ProjectID != nil {
				scope = "project " + shortID(*entry.ProjectID)
			}
			fmt.Fprintf(out, "Added %s → %s (%s, %s)\n",
				entry.Source, entry.Target, entry.Type, scope)
			return nil
		},
	}

	cmd.Flags().StringVar(&project, "project", "", "apply only to this project")
	cmd.Flags().StringVar(&termType, "type", "term",
		"entry type: term, character, place, org, work or honorific")
	cmd.Flags().StringVar(&note, "note", "", "a note shown to the translator")
	cmd.Flags().IntVar(&priority, "priority", 100, "lower wins when two entries conflict")
	cmd.Flags().BoolVar(&disabled, "disabled", false, "add the entry without enabling it")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the entry as JSON")

	return cmd
}

func newGlossaryListCmd(g *globals) *cobra.Command {
	var (
		project string
		all     bool
		asJSON  bool
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List glossary entries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			application, err := g.openApp(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			projectID := ""
			if project != "" {
				id, err := resolveProjectID(cmd, application, project)
				if err != nil {
					return err
				}
				projectID = id
			}

			entries, err := application.Glossary.List(cmd.Context(), projectID, all)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if asJSON {
				encoder := json.NewEncoder(out)
				encoder.SetIndent("", "  ")
				return encoder.Encode(entries)
			}

			if len(entries) == 0 {
				fmt.Fprintln(out, "No glossary entries. Add one with `nikucooker glossary add <source> <target>`.")
				return nil
			}

			t := newTable("SOURCE", "TARGET", "TYPE", "SCOPE", "PRIORITY", "ENABLED")
			for _, entry := range entries {
				scope := "global"
				if entry.ProjectID != nil {
					scope = shortID(*entry.ProjectID)
				}
				t.add(entry.Source, entry.Target, string(entry.Type), scope,
					strconv.Itoa(entry.Priority), boolMark(entry.Enabled))
			}
			return t.render(out)
		},
	}

	cmd.Flags().StringVar(&project, "project", "", "list entries in scope for this project")
	cmd.Flags().BoolVar(&all, "all", false, "include disabled entries")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")

	return cmd
}

func newGlossaryRemoveCmd(g *globals) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a glossary entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			application, err := g.openApp(cmd)
			if err != nil {
				return err
			}
			defer func() { _ = application.Close() }()

			id, err := resolveGlossaryID(cmd, application, args[0])
			if err != nil {
				return err
			}

			entry, err := application.Glossary.Get(cmd.Context(), id)
			if err != nil {
				return err
			}

			if !force {
				return fmt.Errorf(
					"removing %q → %q changes future translations; pass --force to confirm",
					entry.Source, entry.Target)
			}

			if err := application.Glossary.Delete(cmd.Context(), id); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s → %s\n", entry.Source, entry.Target)
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "remove without a confirmation")
	return cmd
}

// resolveGlossaryID accepts a full id, an id prefix, or a source term.
func resolveGlossaryID(cmd *cobra.Command, application *app.App, name string) (string, error) {
	entries, err := application.Glossary.List(cmd.Context(), "", true)
	if err != nil {
		return "", err
	}

	var matches []glossary.Entry
	for _, entry := range entries {
		if entry.ID == name || entry.Source == name {
			return entry.ID, nil
		}
		if len(name) >= 4 && len(entry.ID) >= len(name) && entry.ID[:len(name)] == name {
			matches = append(matches, entry)
		}
	}

	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no glossary entry matches %q", name)
	case 1:
		return matches[0].ID, nil
	default:
		return "", fmt.Errorf("%q matches %d entries; use the full id", name, len(matches))
	}
}

// boolMark renders a boolean as a mark rather than as true/false.
func boolMark(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
