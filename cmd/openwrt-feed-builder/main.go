package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"openwrt-feed-builder/internal/cli"
	"openwrt-feed-builder/internal/feedbuilder"
)

func main() {
	// Ctrl-C / SIGTERM cancel running downloads, SDK builds and the server
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	err := cli.NewRoot().ExecuteContext(ctx)

	stop()

	if err != nil {
		fmt.Fprintln(os.Stderr, feedbuilder.Fail(), err)
		os.Exit(1)
	}
}
