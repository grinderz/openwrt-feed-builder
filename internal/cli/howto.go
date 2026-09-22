package cli

import (
	"github.com/spf13/cobra"

	"openwrt-feed-builder/internal/feedbuilder"
)

func newHowtoCmd(state *app) *cobra.Command {
	return &cobra.Command{
		Use:   "howto",
		Short: "Print how to add the feed on a router (no build)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return feedbuilder.Howto(state.cfg)
		},
	}
}

func newServeCmd(state *app) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Serve the feed tree over HTTP (config `serve` section)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return feedbuilder.Serve(cmd.Context(), state.cfg)
		},
	}
}

func newGenkeyCmd() *cobra.Command {
	var (
		apk            bool
		secret, public string
	)

	cmd := &cobra.Command{
		Use:   "genkey",
		Short: "Generate a signing keypair (usign, or EC for apk with --apk)",
		Long: "Generates the usign keypair opkg feeds are signed with, or with --apk the\n" +
			"ECDSA P-256 keypair of apk feeds (OpenWrt 25.12+). Needs no config.",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if apk {
				return feedbuilder.GenkeyAPK(orDefault(secret, "keys/apk-private.pem"),
					orDefault(public, "keys/apk-public.pem"))
			}

			return feedbuilder.Genkey(cmd.Context(), orDefault(secret, "keys/secret.key"),
				orDefault(public, "keys/public.key"))
		},
	}

	flags := cmd.Flags()
	flags.BoolVar(&apk, "apk", false, "generate the EC keypair for apk feeds (OpenWrt 25.12+) "+
		"instead of the usign one")
	flags.StringVar(&secret, "secret", "", "secret key output path "+
		"(default keys/secret.key, with --apk keys/apk-private.pem)")
	flags.StringVar(&public, "public", "", "public key output path "+
		"(default keys/public.key, with --apk keys/apk-public.pem)")

	return cmd
}

func orDefault(value, def string) string {
	if value == "" {
		return def
	}

	return value
}
