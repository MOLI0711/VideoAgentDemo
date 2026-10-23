package main

import (
	"context"
	"flag"
	"github.com/MOLI0711/MOLIVedio/internal/app"
	"github.com/MOLI0711/MOLIVedio/internal/config"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if e := run(); e != nil {
		slog.Error("worker stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "configs/local.yaml", "configuration path")
	kind := flag.String("kind", "media", "media or agent")
	flag.Parse()
	c, e := config.Load(*path)
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	r, e := app.Open(ctx, c)
	if e != nil {
		return e
	}
	defer r.Close()
	worker, e := r.Worker(*kind)
	if e != nil {
		return e
	}
	slog.Info("worker started", "kind", *kind, "dispatch_mode", c.Workflow.DispatchMode)
	return worker.Run(ctx)
}
