package main

import (
	"context"
	"flag"
	"github.com/MOLI0711/MOLIVedio/internal/app"
	"github.com/MOLI0711/MOLIVedio/internal/config"
	"github.com/MOLI0711/MOLIVedio/internal/eventbus"
	"golang.org/x/sync/errgroup"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if e := run(); e != nil {
		slog.Error("coordinator stopped", "error", e)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "configs/local.yaml", "configuration path")
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
	g, ctx := errgroup.WithContext(ctx)
	if c.Workflow.DispatchMode == "kafka" {
		bus, e := eventbus.New(c, r.Store)
		if e != nil {
			return e
		}
		defer bus.Close()
		g.Go(func() error { return bus.RunProducer(ctx) })
		g.Go(func() error { return bus.RunConsumer(ctx) })
	}
	g.Go(func() error {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-tick.C:
				if _, e := r.Store.DispatchDue(ctx, 100); e != nil {
					return e
				}
				if _, e := r.Store.RecoverExpired(ctx, 100, c.Workflow.MaxFailureRetries); e != nil {
					return e
				}
				if _, e := r.Store.ExpireDeadlines(ctx, 100); e != nil {
					return e
				}
				if c.Workflow.DispatchMode == "db_dev" {
					if _, e := r.Store.DispatchLocal(ctx, 100); e != nil {
						return e
					}
				}
			}
		}
	})
	return g.Wait()
}
