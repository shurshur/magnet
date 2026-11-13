// Command magnet finds and downloads .torrent files from the BitTorrent DHT.
package main

import (
	"os"

	"github.com/shurshur/magnet/internal/cli"
)

// version is injected at build time:
//
//	go build -ldflags "-X main.version=v1.0.0" ./cmd/magnet
var version = "dev"

func main() {
	os.Exit(cli.Run(os.Args[1:], version))
}
