package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/DMarby/picsum-photos/internal/api"
	"github.com/DMarby/picsum-photos/internal/cmd"
	"github.com/DMarby/picsum-photos/internal/hmac"
	"github.com/DMarby/picsum-photos/internal/metrics"
	"github.com/DMarby/picsum-photos/internal/tracing/test"

	postgresDatabase "github.com/DMarby/picsum-photos/internal/database/postgres"
	"github.com/DMarby/picsum-photos/internal/health"
	"github.com/DMarby/picsum-photos/internal/logger"

	"github.com/jamiealquiza/envy"
	"go.uber.org/automaxprocs/maxprocs"
	"go.uber.org/zap"
)

// Commandline flags
var (
	// Global
	listen          = flag.String("listen", "", "listen address (tcp host:port or unix socket path)")
	metricsListen   = flag.String("metrics-listen", "127.0.0.1:8082", "metrics listen address")
	rootURL         = flag.String("root-url", "https://picsum.photos", "root url")
	imageServiceURL = flag.String("image-service-url", "https://fastly.picsum.photos", "image service url")
	loglevel        = zap.LevelFlag("log-level", zap.InfoLevel, "log level (default \"info\") (debug, info, warn, error, dpanic, panic, fatal)")

	// Database - Postgres
	databaseURL = flag.String("database-url", "", "postgres connection string (overridden by DATABASE_URL env var)")

	// HMAC
	hmacKey = flag.String("hmac-key", "", "hmac key to use for authentication between services")
)

func main() {
	ctx := context.Background()

	// Parse environment variables (PICSUM_ prefix)
	envy.Parse("PICSUM")

	// Also pick up the standard Railway DATABASE_URL directly
	if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" && *databaseURL == "" {
		*databaseURL = dbURL
	}

	// Parse commandline flags
	flag.Parse()

	// Re-check after flag parse in case DATABASE_URL was set
	if *databaseURL == "" {
		if dbURL := os.Getenv("DATABASE_URL"); dbURL != "" {
			*databaseURL = dbURL
		}
	}

	// Initialize the logger
	log := logger.New(*loglevel)
	defer log.Sync()

	// Initialize tracing
	tracer := test.Tracer(log)

	// Set GOMAXPROCS
	maxprocs.Set(maxprocs.Logger(log.Infof))

	// Set up context for shutting down
	shutdownCtx, shutdown := signal.NotifyContext(ctx, os.Interrupt, os.Kill, syscall.SIGTERM)
	defer shutdown()

	// Initialize the database
	if *databaseURL == "" {
		log.Fatal("database-url is required (set DATABASE_URL env var or -database-url flag)")
	}

	database, err := postgresDatabase.New(ctx, *databaseURL)
	if err != nil {
		log.Fatalf("error initializing database: %s", err)
	}
	defer database.Close()

	// Initialize and start the health checker
	checkerCtx, checkerCancel := context.WithCancel(ctx)
	defer checkerCancel()

	checker := &health.Checker{
		Ctx:      checkerCtx,
		Database: database,
		Log:      log,
	}
	go checker.Run()

	// Start and listen on http
	api := &api.API{
		Database:        database,
		Log:             log,
		Tracer:          tracer,
		RootURL:         *rootURL,
		ImageServiceURL: *imageServiceURL,
		HandlerTimeout:  cmd.HandlerTimeout,
		HMAC: &hmac.HMAC{
			Key: []byte(*hmacKey),
		},
	}
	router, err := api.Router()
	if err != nil {
		log.Fatalf("error initializing router: %s", err)
	}

	server := &http.Server{
		Handler:      router,
		ReadTimeout:  cmd.ReadTimeout,
		WriteTimeout: cmd.WriteTimeout,
		IdleTimeout:  cmd.IdleTimeout,
		ErrorLog:     logger.NewHTTPErrorLog(log),
	}

	// Determine network type: TCP if address contains ":", otherwise Unix socket
	network := "unix"
	if strings.Contains(*listen, ":") {
		network = "tcp"
	} else {
		os.Remove(*listen)
	}

	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, network, *listen)
	if err != nil {
		log.Fatalf("error creating %s listener: %s", network, err.Error())
	}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Errorf("error shutting down the http server: %s", err)
		}
	}()

	log.Infof("http server listening on %s", *listen)

	// Start the metrics http server
	go metrics.Serve(shutdownCtx, log, checker, *metricsListen)

	// Wait for shutdown
	<-shutdownCtx.Done()
	log.Infof("shutting down: %s", shutdownCtx.Err())

	// Shut down http server
	serverCtx, serverCancel := context.WithTimeout(ctx, cmd.WriteTimeout)
	defer serverCancel()
	if err := server.Shutdown(serverCtx); err != nil {
		log.Warnf("error shutting down: %s", err)
	}
}
