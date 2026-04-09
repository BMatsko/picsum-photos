package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/DMarby/picsum-photos/internal/api"
	"github.com/DMarby/picsum-photos/internal/cache/memory"
	"github.com/DMarby/picsum-photos/internal/cmd"
	"github.com/DMarby/picsum-photos/internal/hmac"
	"github.com/DMarby/picsum-photos/internal/image"
	"github.com/DMarby/picsum-photos/internal/image/vips"
	imageapi "github.com/DMarby/picsum-photos/internal/imageapi"
	"github.com/DMarby/picsum-photos/internal/metrics"
	"github.com/DMarby/picsum-photos/internal/tracing/test"

	postgresDatabase "github.com/DMarby/picsum-photos/internal/database/postgres"
	fileStorage "github.com/DMarby/picsum-photos/internal/storage/file"
	"github.com/DMarby/picsum-photos/internal/health"
	"github.com/DMarby/picsum-photos/internal/logger"

	"github.com/jamiealquiza/envy"
	"go.uber.org/automaxprocs/maxprocs"
	"go.uber.org/zap"
)

// Commandline flags
var (
	listen        = flag.String("listen", "", "listen address (tcp host:port or unix socket path)")
	metricsListen = flag.String("metrics-listen", "127.0.0.1:8082", "metrics listen address")
	rootURL       = flag.String("root-url", "", "public root URL of this service")
	storagePath   = flag.String("storage-path", "/data/images", "path to the directory containing image JPEGs")
	databaseURL   = flag.String("database-url", "", "postgres connection string (or set DATABASE_URL)")
	hmacKey       = flag.String("hmac-key", "", "hmac key for signing image URLs")
	workers       = flag.Int("workers", 3, "image processing worker concurrency")
	loglevel      = zap.LevelFlag("log-level", zap.InfoLevel, "log level (debug, info, warn, error)")
)

func main() {
	ctx := context.Background()

	// Parse PICSUM_ prefixed env vars into flags
	envy.Parse("PICSUM")

	// Railway injects DATABASE_URL directly (no prefix) — pick it up explicitly
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
		os.Setenv("PICSUM_DATABASE_URL", dbURL)
	}

	flag.Parse()

	// Re-check after parse
	if *databaseURL == "" {
		*databaseURL = os.Getenv("DATABASE_URL")
	}

	log := logger.New(*loglevel)
	defer log.Sync()

	tracer := test.Tracer(log)

	maxprocs.Set(maxprocs.Logger(log.Infof))

	shutdownCtx, shutdown := signal.NotifyContext(ctx, os.Interrupt, os.Kill, syscall.SIGTERM)
	defer shutdown()

	// ── Database ──────────────────────────────────────────────────────────────
	if *databaseURL == "" {
		log.Fatal("DATABASE_URL is required")
	}
	db, err := postgresDatabase.New(ctx, *databaseURL)
	if err != nil {
		log.Fatalf("error initializing database: %s", err)
	}
	defer db.Close()

	// ── Storage ───────────────────────────────────────────────────────────────
	storage, err := fileStorage.New(*storagePath)
	if err != nil {
		log.Fatalf("error initializing storage (check PICSUM_STORAGE_PATH, got %q): %s", *storagePath, err)
	}

	// ── Image processor ───────────────────────────────────────────────────────
	cache := memory.New()
	defer cache.Shutdown()

	h := &hmac.HMAC{Key: []byte(*hmacKey)}

	imageProcessor, err := vips.New(shutdownCtx, log, tracer, *workers, image.NewCache(tracer, cache, storage))
	if err != nil {
		log.Fatalf("error initializing image processor: %s", err)
	}

	// ── Health checker ────────────────────────────────────────────────────────
	checkerCtx, checkerCancel := context.WithCancel(ctx)
	defer checkerCancel()

	checker := &health.Checker{
		Ctx:      checkerCtx,
		Database: db,
		Storage:  storage,
		Cache:    cache,
		Log:      log,
	}
	go checker.Run()

	// ── Resolve root URL ──────────────────────────────────────────────────────
	// Railway exposes RAILWAY_PUBLIC_DOMAIN automatically
	if *rootURL == "" {
		if domain := os.Getenv("RAILWAY_PUBLIC_DOMAIN"); domain != "" {
			*rootURL = "https://" + domain
		}
	}
	if *rootURL == "" {
		if port := os.Getenv("PORT"); port != "" {
			*rootURL = fmt.Sprintf("http://localhost:%s", port)
		} else {
			*rootURL = "http://localhost:8080"
		}
	}

	// ── Image API (processes images inline — no redirect) ────────────────────
	imgAPI := imageapi.NewAPI(imageProcessor, log, tracer, cmd.HandlerTimeout, h)

	// ── Main API (routing, DB lookup, list, seed, etc.) ───────────────────────
	// We point ImageServiceURL at ourselves so redirect URLs resolve locally.
	mainAPI := &api.API{
		Database:        db,
		Log:             log,
		Tracer:          tracer,
		RootURL:         *rootURL,
		ImageServiceURL: *rootURL, // same host — image requests loop back to imgAPI
		HandlerTimeout:  cmd.HandlerTimeout,
		HMAC:            h,
	}

	mainRouter, err := mainAPI.Router()
	if err != nil {
		log.Fatalf("error initializing router: %s", err)
	}

	// Combine: image processing routes (/id/{id}/{w}/{h}) handled by imgAPI,
	// everything else by mainAPI.
	mux := http.NewServeMux()
	mux.Handle("/id/", imgAPI.Router())
	mux.Handle("/", mainRouter)

	server := &http.Server{
		Handler:      mux,
		ReadTimeout:  cmd.ReadTimeout,
		WriteTimeout: cmd.WriteTimeout,
		IdleTimeout:  cmd.IdleTimeout,
		ErrorLog:     logger.NewHTTPErrorLog(log),
	}

	// ── Listen ────────────────────────────────────────────────────────────────
	listenAddr := *listen
	if listenAddr == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		listenAddr = "0.0.0.0:" + port
	}

	network := "unix"
	if strings.Contains(listenAddr, ":") {
		network = "tcp"
	} else {
		os.Remove(listenAddr)
	}

	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, network, listenAddr)
	if err != nil {
		log.Fatalf("error creating listener: %s", err)
	}

	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Errorf("error shutting down http server: %s", err)
		}
	}()

	log.Infof("picsum-photos listening on %s (root: %s)", listenAddr, *rootURL)

	go metrics.Serve(shutdownCtx, log, checker, *metricsListen)

	<-shutdownCtx.Done()
	log.Infof("shutting down: %s", shutdownCtx.Err())

	serverCtx, serverCancel := context.WithTimeout(ctx, cmd.WriteTimeout)
	defer serverCancel()
	if err := server.Shutdown(serverCtx); err != nil {
		log.Warnf("error shutting down: %s", err)
	}
}
