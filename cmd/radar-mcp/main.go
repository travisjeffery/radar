// Command radar-mcp is the hosted Radar review MCP server: it runs
// Radar's CI review on any commit pushed to an allowlisted repository, for
// people signed in through the auth proxy in front of it. It is
// report-only. docs/mcp.md has the design, the Codex isolation and setup.
//
//	radar-mcp serve            the MCP server (the server container)
//	radar-mcp runner           runs reviews (the reviewer container)
//	radar-mcp check-isolation  the reviewer container's readiness probe
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// version is set at build time; it is part of every stored review's key.
var version = "dev"

const (
	defaultListenAddress   = "127.0.0.1:8080"
	defaultMetricsAddress  = ":9090"
	defaultBrokerAddress   = "127.0.0.1:8081"
	defaultRunnerSocket    = "/run/radar-runner/runner.sock"
	defaultPacksDir        = "/opt/radar/review-packs"
	defaultResultsPrefix   = "reviews/v1"
	defaultDrainTimeout    = 20 * time.Minute
	defaultConfigPath      = "/etc/radar-mcp/config.json"
	defaultGitHubAppKeyEnv = "RADAR_MCP_GITHUB_APP_KEY_FILE"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		return runServe(logger)
	case "runner":
		return runRunner(logger)
	case "check-isolation":
		return runCheckIsolation()
	case "version":
		fmt.Println(version)
		return 0
	default:
		fmt.Fprintln(os.Stderr, "usage: radar-mcp [serve|runner|check-isolation|version]")
		return 2
	}
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func drainTimeout() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("RADAR_MCP_DRAIN_TIMEOUT")); err == nil && d > 0 {
		return d
	}
	return defaultDrainTimeout
}

func runServe(logger *slog.Logger) int {
	srv, servers, cancelReviews, err := newServe(logger)
	if err != nil {
		logger.Error("starting radar-mcp", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errs := make(chan error, len(servers))
	for name, s := range servers {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- fmt.Errorf("%s listener: %w", name, err)
			}
		}()
	}
	logger.Info("radar-mcp serving", "version", version, "repositories", srv.config.repositories())
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errs:
		logger.Error("radar-mcp", "error", serveErr)
	}

	// Stop starting reviews and let running ones finish (the pod's grace
	// period must outlast the drain); status calls are still answered
	// meanwhile. Then close the listeners, the broker last of all.
	srv.drain(drainTimeout(), cancelReviews)
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, name := range []string{"MCP", "metrics", "broker"} {
		_ = servers[name].Shutdown(shutdown)
	}
	if serveErr != nil {
		return 1
	}
	return 0
}

func newServe(logger *slog.Logger) (*server, map[string]*http.Server, context.CancelFunc, error) {
	cfg, err := loadServerConfig(envOr("RADAR_MCP_CONFIG", defaultConfigPath))
	if err != nil {
		return nil, nil, nil, err
	}
	proxyToken := os.Getenv("RADAR_MCP_PROXY_TOKEN")
	if len(proxyToken) < 32 {
		return nil, nil, nil, errors.New("RADAR_MCP_PROXY_TOKEN, the secret the auth proxy sends, must be at least 32 characters")
	}
	bucket := os.Getenv("RADAR_MCP_RESULTS_BUCKET")
	if bucket == "" {
		return nil, nil, nil, errors.New("RADAR_MCP_RESULTS_BUCKET is required")
	}
	gh, err := loadGitHubApp(os.Getenv("RADAR_MCP_GITHUB_APP_ID"), os.Getenv(defaultGitHubAppKeyEnv))
	if err != nil {
		return nil, nil, nil, err
	}
	packsDigest, err := digestDir(envOr("RADAR_MCP_REVIEW_PACKS", defaultPacksDir))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("reading review packs: %w", err)
	}
	awsCfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("loading AWS config: %w", err)
	}
	reviewerCreds, err := reviewCredentials(awsCfg)
	if err != nil {
		return nil, nil, nil, err
	}
	brokerAddr := envOr("RADAR_MCP_BROKER_LISTEN_ADDRESS", defaultBrokerAddress)
	if host, _, err := net.SplitHostPort(brokerAddr); err != nil || host != "127.0.0.1" {
		return nil, nil, nil, errors.New("RADAR_MCP_BROKER_LISTEN_ADDRESS must be on 127.0.0.1")
	}
	pod, _ := os.Hostname()
	reviews, cancelReviews := context.WithCancel(context.Background())
	runner := newRunnerClient(envOr("RADAR_MCP_RUNNER_SOCKET", defaultRunnerSocket))
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	srv := &server{
		config: cfg, version: version, packsDigest: packsDigest, pod: envOr("POD_NAME", pod),
		brokerURL: "http://" + brokerAddr + "/credentials", proxyToken: proxyToken,
		github:  gh,
		store:   &s3Store{client: s3.NewFromConfig(awsCfg), bucket: bucket, prefix: envOr("RADAR_MCP_RESULTS_PREFIX", defaultResultsPrefix)},
		runner:  runner,
		broker:  newCredentialBroker(reviewerCreds),
		admit:   newAdmission(cfg.Limits),
		metrics: newMetrics(registry),
		logger:  logger,
		now:     time.Now,
		reviews: reviews,
		done:    map[reviewKey]chan struct{}{},
	}
	srv.registerTools()

	mcpMux := http.NewServeMux()
	mcpMux.Handle("/mcp", srv.handler())
	brokerMux := http.NewServeMux()
	brokerMux.Handle("/credentials", srv.broker)
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	metricsMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	metricsMux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := runner.ready(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	servers := map[string]*http.Server{
		// WriteTimeout is left unset: radar_review holds a call open up to
		// ReviewWait.
		"MCP":     {Addr: envOr("RADAR_MCP_LISTEN_ADDRESS", defaultListenAddress), Handler: mcpMux, ReadHeaderTimeout: 10 * time.Second},
		"broker":  {Addr: brokerAddr, Handler: brokerMux, ReadHeaderTimeout: 10 * time.Second},
		"metrics": {Addr: envOr("RADAR_MCP_METRICS_LISTEN_ADDRESS", defaultMetricsAddress), Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second},
	}
	return srv, servers, cancelReviews, nil
}
