package main

import (
	"os"

	"openwrt-feed-builder/internal/feedbuilder"
)

func main() {
	os.Exit(feedbuilder.Run(os.Args[1:]))
}
