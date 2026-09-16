package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/iaia/telegramgw/internal/releasemanager"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := releasemanager.Execute(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
