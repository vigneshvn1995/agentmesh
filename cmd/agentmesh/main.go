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

	"github.com/redis/go-redis/v9"

	v1 "agentmesh/api/v1"
	"agentmesh/internal/budget"
	"agentmesh/internal/cache"
	"agentmesh/internal/config"
	"agentmesh/internal/guardrail"
	"agentmesh/internal/proxy"
	"agentmesh/internal/telemetry"
)

func main() {
	// --- Structured JSON logger -------------------------------------------
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	// --- CLI flags ----------------------------------------------------------
	configPath := flag.String("config", "agentmesh.yaml", "path to agentmesh YAML config file")
	flag.Parse()

	// --- Config -------------------------------------------------------------
	lc, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load configuration", "path", *configPath, "error", err)
		os.Exit(1)
	}
	slog.Info("configuration loaded", "version", lc.Config.Version, "tenants", len(lc.Config.Tenants))

	// --- Root context with cancellation ------------------------------------
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- OpenTelemetry provider --------------------------------------------
	shutdown, err := telemetry.InitProvider(ctx, lc.Config)
	if err != nil {
		slog.Error("failed to initialise OpenTelemetry", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := shutdown(context.Background()); err != nil {
			slog.Error("OpenTelemetry shutdown error", "error", err)
		}
	}()

	// --- Guardrail breaker -------------------------------------------------
	// Read window size and limit from config; fall back to safe defaults.
	breakerWindow := lc.Config.Guardrails.LoopDetection.WindowSize.Duration
	if breakerWindow <= 0 {
		breakerWindow = 5 * time.Minute
	}
	breakerLimit := lc.Config.Guardrails.LoopDetection.MaxIdenticalHash
	if breakerLimit <= 0 {
		breakerLimit = 3
	}
	breaker := guardrail.NewBreaker(breakerWindow, breakerLimit)

	// Background goroutine sweeps expired entries every 5 minutes to prevent
	// unbounded growth of the breaker's internal history map.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				breaker.Sweep()
			}
		}
	}()

	// --- Budget tracker ----------------------------------------------------
	rdb := redis.NewClient(&redis.Options{
		Addr:     lc.Config.Redis.Address,
		Password: lc.Config.Redis.Password,
		DB:       lc.Config.Redis.DB,
		PoolSize: lc.Config.Redis.PoolSize,
	})

	failureMode := lc.Config.Redis.FailureMode
	if failureMode == "" {
		failureMode = v1.FailOpen
	}
	tracker := budget.NewTracker(rdb, &lc.Config.Budget, failureMode)

	// Close the Redis connection pool on shutdown so in-flight connections are
	// returned cleanly and the budget middleware's async goroutines can finish.
	defer func() {
		if err := rdb.Close(); err != nil {
			slog.Error("Redis client close error", "error", err)
		}
	}()

	// --- Semantic cache ----------------------------------------------------
	// A no-op pass-through is the default so RegisterChain always receives a
	// valid middleware regardless of whether caching is configured.
	cacheMiddleware := func(next http.Handler) http.Handler { return next }

	if lc.Config.Cache != nil && lc.Config.Cache.Enabled {
		qdrantStore, err := cache.NewQdrantStore(
			os.Getenv("QDRANT_ENDPOINT"),
			os.Getenv("QDRANT_API_KEY"),
			"agentmesh_cache",
			lc.Config.Cache.TTL.Duration,
		)
		if err != nil {
			slog.Error("failed to initialise Qdrant store", "error", err)
			os.Exit(1)
		}

		embedder := cache.NewOpenAIEmbedder(
			"https://api.openai.com/v1/embeddings",
			os.Getenv("OPENAI_API_KEY"),
			"text-embedding-3-small",
			lc.Config.Budget.RequestTimeout.Duration,
		)

		// Read the similarity threshold from config; the loader already
		// applied the 0.90 default when the field was unset.
		threshold := lc.Config.Cache.SimilarityThreshold

		cacheMiddleware = cache.Middleware(qdrantStore, embedder, cache.Config{
			SimilarityThreshold: threshold,
		})
		slog.Info("semantic cache enabled", "similarity_threshold", threshold)
	}

	// --- Proxy server -------------------------------------------------------
	srv, err := proxy.NewServer(lc)
	if err != nil {
		slog.Error("failed to create proxy server", "error", err)
		os.Exit(1)
	}

	// Wire the middleware chain:
	// AuthMiddleware → Guardrail → Cache → Budget → HandleProxy
	// Cache hits short-circuit the chain before Budget, meaning zero tokens
	// are deducted from Redis and the upstream LLM is never called.
	srv.RegisterChain(
		srv.GuardrailMiddleware(breaker),
		cacheMiddleware,
		budget.Middleware(tracker),
	)

	// --- Signal handler -----------------------------------------------------
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("graceful shutdown initiated", "signal", sig.String())
		cancel()
	}()

	// --- Start proxy and admin servers concurrently -------------------------
	serverErrCh := make(chan error, 2)

	go func() {
		slog.Info("admin server listening", "port", lc.Config.Server.AdminPort)
		if err := srv.StartAdmin(ctx, func(ctx context.Context) error {
			return rdb.Ping(ctx).Err()
		}); err != nil {
			serverErrCh <- fmt.Errorf("admin server: %w", err)
			return
		}
		serverErrCh <- nil
	}()

	go func() {
		slog.Info("proxy listening", "port", lc.Config.Server.ProxyPort)
		if err := srv.Start(ctx); err != nil {
			serverErrCh <- fmt.Errorf("proxy server: %w", err)
			return
		}
		serverErrCh <- nil
	}()

	// Drain both servers. On the first non-nil error cancel the context to
	// trigger a graceful shutdown of the other server, then exit.
	for range 2 {
		if err := <-serverErrCh; err != nil {
			slog.Error("server error", "error", err)
			cancel()
			os.Exit(1)
		}
	}
}
