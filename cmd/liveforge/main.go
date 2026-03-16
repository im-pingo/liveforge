package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/im-pingo/liveforge/config"
)

var version = "dev"

func main() {
	configPath := flag.String("c", "configs/streamserver.yaml", "config file path")
	showVersion := flag.Bool("v", false, "show version")
	flag.Parse()

	if *showVersion {
		fmt.Printf("liveforge %s\n", version)
		os.Exit(0)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	log.Printf("liveforge %s starting, server name: %s", version, cfg.Server.Name)
}
