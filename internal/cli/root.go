// Package cli wires the CLI commands (cobra) onto the feedbuilder package.
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"openwrt-feed-builder/internal/feedbuilder"
)

var errBothColorFlags = errors.New("--color and --no-color are mutually exclusive")

// skipConfigAnnotation marks commands that run without a config file.
const skipConfigAnnotation = "skip-config"

// app is what the commands share: the config path and, once loaded, the config.
type app struct {
	cfgPath        string
	cfg            *feedbuilder.Config
	color, noColor bool
}

// NewRoot builds the command tree.
func NewRoot() *cobra.Command {
	state := &app{}

	root := &cobra.Command{
		Use:   "openwrt-feed-builder",
		Short: "Collect OpenWrt packages from HTTP sources and build a signed custom feed",
		Long: "Collects .ipk/.apk packages from HTTP sources and builds a signed OpenWrt\n" +
			"custom feed: opkg for 24.10 and older, apk for 25.12+. See config.example.yaml.",
		Version:       feedbuilder.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if state.color && state.noColor {
				return errBothColorFlags
			}

			feedbuilder.SetColor(state.color || (!state.noColor && feedbuilder.AutoColor()))

			if cmd.Annotations[skipConfigAnnotation] != "" || cmd.Name() == "help" {
				return nil
			}

			cfg, err := feedbuilder.LoadConfig(state.cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			state.cfg = cfg

			return nil
		},
	}
	root.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	flags := root.PersistentFlags()
	flags.StringVarP(&state.cfgPath, "config", "c", "config.yaml", "path to the config")
	flags.BoolVar(&state.color, "color", false, "colour the output (default: when on a terminal, unless NO_COLOR)")
	flags.BoolVar(&state.noColor, "no-color", false, "plain output, no escape sequences")

	root.AddCommand(
		newBuildCmd(state),
		newIndexDiffCmd(state),
		newSignCmd(state),
		newVerifyCmd(state),
		newHowtoCmd(state),
		newServeCmd(state),
		newGenkeyCmd(),
	)

	return root
}
