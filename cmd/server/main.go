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
	"time"

	"github.com/cloud-exit/argocd-cluster-proxy/pkg/server"
)

var version = "dev"

func main() {
	var (
		addr         = flag.String("addr", ":8443", "public listen address (agent /connect)")
		internalAddr = flag.String("internal-addr", ":8080", "internal listen address (/tunnel, /metrics)")
		clustersFile = flag.String("clusters", "", "path to clusters config JSON file")
		rateLimit    = flag.Float64("rate-limit", 0, "max proxy requests per second (0 = unlimited)")
		tlsCert      = flag.String("tls-cert", "", "path to TLS certificate (enables TLS on public port)")
		tlsKey       = flag.String("tls-key", "", "path to TLS private key")
		tlsClientCA  = flag.String("tls-client-ca", "", "path to CA cert for client verification (enables mTLS)")
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

	reg := server.NewRegistry(clusters)
	var opts []server.Option
	if *rateLimit > 0 {
		opts = append(opts, server.WithRateLimit(*rateLimit))
	}
	srv := server.New(reg, logger, opts...)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	useTLS := *tlsCert != "" && *tlsKey != ""

	logger.Info("starting proxy server",
		"public_addr", *addr,
		"internal_addr", *internalAddr,
		"clusters", len(clusters),
		"tls", useTLS,
		"mtls", useTLS && *tlsClientCA != "",
		"version", version,
	)

	errCh := make(chan error, 2)
	// Internal port always serves plain HTTP — it's ClusterIP-only and
	// needs to be reachable by kubelet probes and Prometheus without TLS.
	go func() { errCh <- server.ListenAndServe(ctx, *internalAddr, srv.InternalHandler()) }()
	if useTLS {
		tc := server.TLSConfig{
			CertFile: *tlsCert,
			KeyFile:  *tlsKey,
			CAFile:   *tlsClientCA,
		}
		go func() { errCh <- server.ListenAndServeTLS(ctx, *addr, srv.PublicHandler(), tc) }()
	} else {
		go func() { errCh <- server.ListenAndServe(ctx, *addr, srv.PublicHandler()) }()
	}

	// Wait for the first server to return (shutdown or error).
	if err := <-errCh; err != nil {
		logger.Error("server error", "err", err)
	}
	srv.Drain(10 * time.Second)
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
	defer func() { _ = f.Close() }()

	var clusters []server.ClusterConfig
	if err := json.NewDecoder(f).Decode(&clusters); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return clusters, nil
}
