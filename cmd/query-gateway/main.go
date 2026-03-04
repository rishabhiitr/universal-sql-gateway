package main

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/trace/noop"
	"go.uber.org/zap"

	"github.com/rishabhm/universal-sql-query-layer/internal/cache"
	"github.com/rishabhm/universal-sql-query-layer/internal/connectors"
	githubconnector "github.com/rishabhm/universal-sql-query-layer/internal/connectors/github"
	jiraconnector "github.com/rishabhm/universal-sql-query-layer/internal/connectors/jira"
	"github.com/rishabhm/universal-sql-query-layer/internal/entitlements"
	"github.com/rishabhm/universal-sql-query-layer/internal/gateway"
	"github.com/rishabhm/universal-sql-query-layer/internal/planner"
	"github.com/rishabhm/universal-sql-query-layer/internal/ratelimit"
	"github.com/rishabhm/universal-sql-query-layer/pkg/middleware"
	"github.com/rishabhm/universal-sql-query-layer/pkg/tracing"
)

func main() {
	logger, _ := zap.NewProduction()
	defer logger.Sync()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	policyPath := envOrDefault("POLICY_PATH", "configs/policy.yaml")
	policy, err := entitlements.LoadPolicy(policyPath)
	if err != nil {
		logger.Fatal("failed to load policy", zap.Error(err))
	}

	reg := prometheus.NewRegistry()
	metrics := middleware.NewMetrics(reg)
	cacheStore := cache.New(30 * time.Second)
	defer cacheStore.Stop()

	// Init distributed tracing to Jaeger. Fall back to noop if unavailable.
	otelEndpoint := envOrDefault("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	tracer, shutdownTracer, err := tracing.Init(ctx, otelEndpoint)
	if err != nil {
		logger.Warn("tracing init failed, using noop tracer", zap.Error(err))
		tracer = noop.NewTracerProvider().Tracer(tracing.ServiceName)
		shutdownTracer = func(context.Context) error { return nil }
	}
	defer shutdownTracer(context.Background())

	limiter := ratelimit.New(
		ratelimit.Config{RatePerSecond: 20, Burst: 40},
		map[string]ratelimit.Config{
			"github": {RatePerSecond: 2, Burst: 2}, // demo: tight limits so burst scenario triggers 429s
			"jira":   {RatePerSecond: 8, Burst: 16},
		},
		map[string]map[string]ratelimit.Config{
			// t-load: high-throughput tenant used by the load test — relaxed per-connector limits
			// so the test measures gateway/planner latency, not rate-limit rejections.
			"t-load": {
				"github": {RatePerSecond: 500, Burst: 1000},
				"jira":   {RatePerSecond: 500, Burst: 1000},
			},
		},
	)
	entitlementEngine := entitlements.NewEngine(policy)

	connectorRegistry := connectors.NewRegistry(
		githubconnector.New(80*time.Millisecond),
		jiraconnector.New(120*time.Millisecond),
	)
	queryParser := planner.NewParser()
	queryExecutor := planner.NewExecutor(connectorRegistry, entitlementEngine, limiter, cacheStore, 2*time.Minute, tracer)
	queryHandler := gateway.NewHandler(queryParser, queryExecutor, logger)

	secret := []byte(envOrDefault("JWT_SECRET", "dev-secret"))
	r := chi.NewRouter()
	r.Use(metrics.HTTPMiddleware)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/query-ui", http.StatusTemporaryRedirect)
	})
	r.Get("/query-ui", queryHandler.QueryUI)
	r.Get("/healthz", queryHandler.Healthz)
	r.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	r.Group(func(private chi.Router) {
		private.Use(middleware.Auth(secret))
		private.Post("/v1/query", queryHandler.Query)
	})

	server := &http.Server{
		Addr:         envOrDefault("HTTP_ADDR", ":8080"),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		logger.Info("query gateway listening", zap.String("addr", server.Addr))
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal("gateway server failed", zap.Error(err))
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", zap.Error(err))
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
