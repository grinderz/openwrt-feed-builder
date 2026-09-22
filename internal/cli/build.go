package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"openwrt-feed-builder/internal/feedbuilder"
)

func newBuildCmd(state *app) *cobra.Command {
	var (
		opts feedbuilder.BuildOptions
		only string
	)

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Collect the sources and build (merge into) the feed tree",
		Long: "Resolves every source, routes the packages into the feed tree and indexes\n" +
			"the touched feed dirs. Incremental by default: unchanged packages stay\n" +
			"untouched, superseded versions of shipped packages are pruned.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Only = splitList(only)

			return feedbuilder.Build(cmd.Context(), state.cfg, opts)
		},
	}

	flags := cmd.Flags()
	flags.BoolVar(&opts.Refresh, "refresh", false, "ignore cache, re-download everything")
	flags.BoolVar(&opts.Full, "full", false, "rebuild the output tree from scratch "+
		"(default merges incrementally: unchanged packages untouched, only "+
		"superseded versions of shipped packages removed)")
	flags.StringVar(&only, "only", "", "build only sources whose type or name matches "+
		"(comma-separated globs, e.g. \"sdk\" or \"amnezia*,ssclash\")")
	flags.BoolVar(&opts.Sign, "sign", false, "sign the touched indexes while building "+
		"(default off; the sign command signs the whole tree afterwards)")
	flags.BoolVar(&opts.Reindex, "reindex", false, "regenerate the index of every feed dir, "+
		"not just the touched ones (use after an index-format change)")
	flags.StringVar(&opts.IndexScript, "index-script", "", "generate indexes with the official "+
		"ipkg-make-index.sh at PATH instead of the native indexer "+
		"(debugging/comparison; e.g. tools/ipkg-make-index.sh)")

	return cmd
}

// splitList parses a comma-separated flag value, dropping empty entries.
func splitList(value string) []string {
	var out []string

	for part := range strings.SplitSeq(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

func newIndexDiffCmd(state *app) *cobra.Command {
	var script string

	cmd := &cobra.Command{
		Use:   "indexdiff",
		Short: "Diff the native opkg indexes against the official ipkg-make-index.sh",
		Long: "Regenerates every opkg feed index twice — natively and with the official\n" +
			"ipkg-make-index.sh — and prints a unified diff per feed dir. Read-only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return feedbuilder.IndexDiff(cmd.Context(), state.cfg, script)
		},
	}
	cmd.Flags().StringVar(&script, "script", "tools/ipkg-make-index.sh", "path to the official ipkg-make-index.sh")

	return cmd
}
