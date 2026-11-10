package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"codearts2api-panel/internal/gateway"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := gateway.Run(*cfgPath, ctx); err != nil {
		log.Fatalf("gateway: %v", err)
	}
}
