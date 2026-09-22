package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/AhsokaTano26/NikuCooker/internal/config"
)

// newConfigCmd builds `nikucooker config`.
//
// It exists because "set server.allow_path_source to enable it" is not advice a
// user can act on until they know which file to edit, and until recently the
// answer was "none, unless you passed --config" — a dead end that surfaced as a
// refusal in the web interface with nowhere to go.
func newConfigCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and create the configuration file",
		Long: `The configuration file is optional: the defaults are a complete, working
setup. This command reports which file would be read, and can write a starter
one to get you started.`,
	}

	cmd.AddCommand(newConfigPathCmd(g), newConfigInitCmd(g))

	return cmd
}

// configTarget is the file a command should report or write.
//
// Explicit flag, then environment, then the default path — the same order the
// server resolves, so `config path` cannot answer for a different file than the
// one that would actually be read.
func configTarget(g *globals) string {
	if g.configPath != "" {
		return g.configPath
	}
	if fromEnv := config.ConfigFileFromEnv(os.Environ()); fromEnv != "" {
		return fromEnv
	}
	return config.DefaultPath
}

func newConfigPathCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the configuration file that would be read",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			target := configTarget(g)

			out := cmd.OutOrStdout()
			fmt.Fprintln(out, target)

			// Where it came from matters as much as which file it is: a user who
			// set NIKUCOOKER_CONFIG months ago and forgot will otherwise edit the
			// file this prints and be confused when nothing changes.
			switch {
			case g.configPath != "":
				fmt.Fprintln(out, "(from --config)")
			case config.ConfigFileFromEnv(os.Environ()) != "":
				fmt.Fprintln(out, "(from NIKUCOOKER_CONFIG)")
			default:
				fmt.Fprintln(out, "(the default path)")
			}

			if _, err := os.Stat(target); err == nil {
				fmt.Fprintln(out, "This file exists and is being read.")
			} else if errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintln(out, "This file does not exist. The defaults are in use;")
				fmt.Fprintln(out, "create it with `nikucooker config init` to change something.")
			} else {
				return fmt.Errorf("checking %s: %w", target, err)
			}
			return nil
		},
	}
}

func newConfigInitCmd(g *globals) *cobra.Command {
	var (
		force  bool
		stdout bool
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a starter configuration file",
		Long: `Writes every setting worth changing, commented, with the defaults in place.
Uncomment a line to change it; the file is checked before it is written, so a
starter file this command produces always loads.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()

			if stdout {
				fmt.Fprint(out, starterConfig)
				return nil
			}

			target := configTarget(g)

			// Refusing to clobber is the whole safety property here: this file is
			// the only place a user's settings live, and a command that silently
			// replaces it would destroy work that took a while to get right.
			if _, err := os.Stat(target); err == nil && !force {
				return fmt.Errorf("%s already exists; pass --force to overwrite it, or run `nikucooker config init --stdout` to see what would be written", target)
			}

			if err := writeStarter(target); err != nil {
				return err
			}

			fmt.Fprintf(out, "wrote %s\n", target)
			fmt.Fprintln(out, "edit it, then restart the server for changes to take effect")
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite the file if it already exists")
	cmd.Flags().BoolVar(&stdout, "stdout", false, "print the file instead of writing it")

	return cmd
}

// writeStarter writes the starter configuration, after proving it is loadable.
//
// The template names keys by hand, so it can drift from the structures it
// documents — and a config file the program cannot start on is a worse outcome
// than no file at all. Loading it first turns that from a user-visible breakage
// into an error the author sees.
func writeStarter(path string) error {
	if err := verifyStarter(starterConfig); err != nil {
		return fmt.Errorf("refusing to write %s: the built-in starter configuration is invalid, which is a bug: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	if err := os.WriteFile(path, []byte(starterConfig), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// verifyStarter parses and resolves a configuration document the way the server
// would, without touching the filesystem.
func verifyStarter(text string) error {
	layer, err := config.ParseLayer(config.SourceFile, []byte(text))
	if err != nil {
		return err
	}
	_, _, err = config.Load(layer)
	return err
}
