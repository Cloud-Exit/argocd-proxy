package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cloud-exit/argocd-cluster-proxy/pkg/agent"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var version = "dev"

func main() {
	var (
		serverURL       = flag.String("server", "", "proxy server WebSocket URL (e.g. wss://proxy:8080/connect)")
		token           = flag.String("token", "", "authentication token (or TOKEN env var)")
		targetAddr      = flag.String("target", "kubernetes.default.svc:443", "local Kubernetes API address")
		caCertPath      = flag.String("ca-cert", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", "path to CA cert for the local API")
		insecure        = flag.Bool("insecure", false, "skip TLS verification to local API")
		plainTarget     = flag.Bool("plain-target", false, "connect to target without TLS (for testing)")
		saTokenPath     = flag.String("sa-token-path", "/var/run/secrets/kubernetes.io/serviceaccount/token", "path to ServiceAccount token for authenticating to the local K8s API (empty to disable)")
		insecureServer  = flag.Bool("allow-insecure-server", false, "allow plaintext ws:// connection to the proxy server")
		maxRetry        = flag.Duration("max-retry", 60*time.Second, "max reconnect backoff interval")
		healthAddr      = flag.String("health-addr", ":8081", "address for health/metrics HTTP server (empty to disable)")
		logLevel        = flag.String("log-level", "info", "log level")
		showVersion     = flag.Bool("version", false, "print version and exit")
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

	if *serverURL == "" {
		*serverURL = os.Getenv("SERVER_URL")
	}
	if *serverURL == "" {
		logger.Error("server URL required: use -server flag or SERVER_URL env var")
		os.Exit(1)
	}

	tok := *token
	if tok == "" {
		tok = os.Getenv("TOKEN")
	}
	if tok == "" {
		logger.Error("token required: use -token flag or TOKEN env var")
		os.Exit(1)
	}

	a, err := agent.New(agent.Config{
		ServerURL:           *serverURL,
		Token:               tok,
		TargetAddr:          *targetAddr,
		SATokenPath:         *saTokenPath,
		CACertPath:          *caCertPath,
		Insecure:            *insecure,
		PlainTarget:         *plainTarget,
		AllowInsecureServer: *insecureServer,
		MaxRetryInterval:    *maxRetry,
	}, logger)
	if err != nil {
		logger.Error("init agent", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start health/metrics HTTP server.
	if *healthAddr != "" {
		healthMux := http.NewServeMux()
		healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		healthMux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
			if a.IsConnected() {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("not connected"))
			}
		})
		healthMux.Handle("/metrics", promhttp.Handler())
		healthSrv := &http.Server{Addr: *healthAddr, Handler: healthMux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
		go func() {
			if herr := healthSrv.ListenAndServe(); herr != nil && herr != http.ErrServerClosed {
				logger.Error("health server", "err", herr)
			}
		}()
		go func() {
			<-ctx.Done()
			_ = healthSrv.Close()
		}()
		logger.Info("health server started", "addr", *healthAddr)
	}

	logger.Info("starting proxy agent", "server", *serverURL, "target", *targetAddr, "version", version)
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("agent error", "err", err)
		os.Exit(1)
	}
}
