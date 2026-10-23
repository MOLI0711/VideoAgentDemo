package main

import (
	"context"
	"flag"
	"github.com/MOLI0711/MOLIVedio/internal/api"
	"github.com/MOLI0711/MOLIVedio/internal/app"
	"github.com/MOLI0711/MOLIVedio/internal/config"
	"github.com/zeromicro/go-zero/core/service"
	"github.com/zeromicro/go-zero/rest"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

func main() {
	if e := run(); e != nil {
		slog.Error("API stopped", "error", e)
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
	runtime, e := app.Open(ctx, c)
	if e != nil {
		return e
	}
	defer runtime.Close()
	var limiter api.Limiter
	if runtime.Redis != nil {
		limiter = &app.RateLimiter{Client: runtime.Redis, Limit: 60}
	}
	handler := api.New(c, runtime.Store, runtime.Objects, limiter)
	host, portText, e := net.SplitHostPort(c.App.HTTPAddr)
	if e != nil {
		return e
	}
	port, e := strconv.Atoi(portText)
	if e != nil {
		return e
	}
	mode := service.DevMode
	if c.App.Env == "production" {
		mode = service.ProMode
	}
	server, e := rest.NewServer(rest.RestConf{ServiceConf: service.ServiceConf{Name: "video-agent-api", Mode: mode}, Host: host, Port: port, MaxConns: 1000, MaxBytes: 1 << 20, Timeout: 30000, Middlewares: rest.MiddlewaresConf{Trace: true, Prometheus: true, MaxConns: true, Timeout: true, Recover: true, MaxBytes: true}})
	if e != nil {
		return e
	}
	defer server.Stop()
	param := regexp.MustCompile(`\{([^}]+)\}`)
	for _, route := range handler.Routes {
		options := []rest.RouteOption{}
		if strings.HasSuffix(route.Path, "/events") {
			options = append(options, rest.WithSSE())
		}
		server.AddRoute(rest.Route{Method: route.Method, Path: param.ReplaceAllString(route.Path, ":$1"), Handler: handler.ServeHTTP}, options...)
	}
	slog.Info("API listening", "address", c.App.HTTPAddr, "model_mode", c.Model.Mode, "asr_mode", c.ASR.Mode)
	server.Start()
	return nil
}
