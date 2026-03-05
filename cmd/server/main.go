package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloud-exit/argocd-cluster-proxy/pkg/server"
)

var version = "dev"

func main() {
	var (
		addr         = flag.String("addr", ":8080", "listen address")
		clustersFile = flag.String("clusters", "", "path to clusters config JSON file")
		proxyToken   = flag.String("proxy-token", "", "bearer token required on /tunnel/ proxy requests (or PROXY_TOKEN env var)")
		rateLimit    = flag.Float64("rate-limit", 0, "max proxy requests per second (0 = unlimited)")
		logLevel     = flag.String("log-level", "info", "log level (debug, info, warn, error)")
		showVersion  = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "invalid log level: %s\n", *logLevel)
		os.Exit(1)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	clusters, err := loadClusters(*clustersFile)
	if err != nil {
		logger.Error("load clusters", "err", err)
		os.Exit(1)
	}

	pToken := *proxyToken
	if pToken == "" {
		pToken = os.Getenv("PROXY_TOKEN")
	}
	if pToken == "" {
		logger.Warn("no proxy token set: /tunnel/ endpoint is unauthenticated — set -proxy-token or PROXY_TOKEN")
	}

	reg := server.NewRegistry(clusters)
	var opts []server.Option
	if pToken != "" {
		opts = append(opts, server.WithProxyToken(pToken))
	}
	if *rateLimit > 0 {
		opts = append(opts, server.WithRateLimit(*rateLimit))
	}
	srv := server.New(reg, logger, opts...)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("starting proxy server", "addr", *addr, "clusters", len(clusters), "version", version)
	if err := server.ListenAndServe(ctx, *addr, srv.Handler()); err != nil {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
}

func loadClusters(path string) ([]server.ClusterConfig, error) {
	if path == "" {
		// Fall back to CLUSTERS env var.
		raw := os.Getenv("CLUSTERS")
		if raw == "" {
			return nil, fmt.Errorf("no clusters configured: use -clusters flag or CLUSTERS env var")
		}
		var clusters []server.ClusterConfig
		if err := json.Unmarshal([]byte(raw), &clusters); err != nil {
			return nil, fmt.Errorf("parse CLUSTERS env: %w", err)
		}
		return clusters, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var clusters []server.ClusterConfig
	if err := json.NewDecoder(f).Decode(&clusters); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return clusters, nil
}
