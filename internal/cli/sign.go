package cli

import (
	"github.com/spf13/cobra"

	"openwrt-feed-builder/internal/feedbuilder"
)

func newSignCmd(state *app) *cobra.Command {
	return &cobra.Command{
		Use:   "sign",
		Short: "Sign an existing feed tree in place",
		Long: "Signs every index under output_dir (Packages.sig for opkg, the embedded\n" +
			"signature of packages.adb for apk) and regenerates repo.pub / repo-apk.pem /\n" +
			"add.sh with the keys baked in. Nothing else is touched.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return feedbuilder.Sign(cmd.Context(), state.cfg)
		},
	}
}

func newVerifyCmd(state *app) *cobra.Command {
	return &cobra.Command{
		Use:   "verify [dir]",
		Short: "Validate every signature, the served public keys and add.sh",
		Long: "Read-only check of a feed tree (output_dir, or dir when given): every index\n" +
			"must carry a valid signature, repo.pub / repo-apk.pem must match the\n" +
			"configured keys and every add.sh must install them. Fails otherwise.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := ""
			if len(args) > 0 {
				dir = args[0]
			}

			return feedbuilder.Verify(cmd.Context(), state.cfg, dir)
		},
	}
}
