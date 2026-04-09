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
	sftpStorage "github.com/DMarby/picsum-photos/internal/storage/sftp"
	"github.com/DMarby/picsum-photos/internal/admin"
	"github.com/DMarby/picsum-photos/internal/uploadapi"
	"github.com/DMarby/picsum-photos/internal/handler"
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
	storagePath   = flag.String("storage-path", "/data/images", "path to the directory containing image JPEGs (local fallback)")
	sftpHost      = flag.String("sftp-host", "", "SFTP server hostname (enables SFTP storage when set)")
	sftpPort      = flag.String("sftp-port", "22", "SFTP server port")
	sftpUser      = flag.String("sftp-user", "", "SFTP username")
	sftpPassword  = flag.String("sftp-password", "", "SFTP password")
	sftpPath      = flag.String("sftp-path", "/images", "base path on SFTP server")
	databaseURL   = flag.String("database-url", "", "postgres connection string (or set DATABASE_URL)")
	hmacKey       = flag.String("hmac-key", "", "hmac key for signing image URLs")
	adminPassword = flag.String("admin-password", "", "password for the admin UI (or set PICSUM_ADMIN_PASSWORD)")
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

	log.Infof("starting picsum-photos")
	log.Infof("storage-path: %s", *storagePath)
	log.Infof("database-url present: %v", *databaseURL != "")
	log.Infof("hmac-key present: %v", *hmacKey != "")

	tracer := test.Tracer(log)
	maxprocs.Set(maxprocs.Logger(log.Infof))

	shutdownCtx, shutdown := signal.NotifyContext(ctx, os.Interrupt, os.Kill, syscall.SIGTERM)
	defer shutdown()

	// ── Database ──────────────────────────────────────────────────────────────
	if *databaseURL == "" {
		log.Fatal("DATABASE_URL is required — add a Postgres plugin in Railway")
	}
	db, err := postgresDatabase.New(ctx, *databaseURL)
	if err != nil {
		log.Fatalf("error connecting to database: %s", err)
	}
	defer db.Close()
	log.Infof("database connected")

	// ── Storage ───────────────────────────────────────────────────────────────
	var storageProvider interface {
		Get(ctx context.Context, id string) ([]byte, error)
	}
	var sftpProvider *sftpStorage.Provider // kept separate so admin can call Put/Delete

	if *sftpHost != "" {
		log.Infof("connecting to SFTP storage at %s:%s", *sftpHost, *sftpPort)
		sftpProvider, err = sftpStorage.New(sftpStorage.Config{
			Host:     *sftpHost,
			Port:     *sftpPort,
			User:     *sftpUser,
			Password: *sftpPassword,
			BasePath: *sftpPath,
		})
		if err != nil {
			log.Fatalf("error connecting to SFTP: %s", err)
		}
		defer sftpProvider.Close()
		storageProvider = sftpProvider
		log.Infof("SFTP storage connected at %s%s", *sftpHost, *sftpPath)
	} else {
		// Local file storage fallback
		if err := os.MkdirAll(*storagePath, 0755); err != nil {
			log.Fatalf("error creating storage directory %q: %s", *storagePath, err)
		}
		var fileProvider *fileStorage.Provider
		fileProvider, err = fileStorage.New(*storagePath)
		if err != nil {
			log.Fatalf("error initializing storage at %q: %s", *storagePath, err)
		}
		storageProvider = fileProvider
		log.Infof("local file storage initialized at %s", *storagePath)
	}

	// ── Image processor ───────────────────────────────────────────────────────
	cache := memory.New()
	defer cache.Shutdown()

	h := &hmac.HMAC{Key: []byte(*hmacKey)}

	imageProcessor, err := vips.New(shutdownCtx, log, tracer, *workers, image.NewCache(tracer, cache, storageProvider))
	if err != nil {
		log.Fatalf("error initializing image processor: %s", err)
	}
	log.Infof("image processor initialized")

	// ── Health checker ────────────────────────────────────────────────────────
	checkerCtx, checkerCancel := context.WithCancel(ctx)
	defer checkerCancel()

	checker := &health.Checker{
		Ctx:      checkerCtx,
		Database: db,
		Storage:  storageProvider,
		Cache:    cache,
		Log:      log,
	}
	go checker.Run()

	// ── Resolve root URL ──────────────────────────────────────────────────────
	if *rootURL == "" {
		if domain := os.Getenv("RAILWAY_PUBLIC_DOMAIN"); domain != "" {
			*rootURL = "https://" + domain
		}
	}
	if *rootURL == "" {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		*rootURL = fmt.Sprintf("http://localhost:%s", port)
	}
	log.Infof("root URL: %s", *rootURL)

	// ── Admin UI ─────────────────────────────────────────────────────────────
	if *adminPassword == "" {
		*adminPassword = os.Getenv("PICSUM_ADMIN_PASSWORD")
	}
	adminUI, err := admin.New(db, *storagePath, sftpProvider, *adminPassword, *rootURL)
	if err != nil {
		log.Fatalf("error initializing admin UI: %s", err)
	}

	// ── Routers ───────────────────────────────────────────────────────────────
	imgAPI := imageapi.NewAPI(imageProcessor, log, tracer, cmd.HandlerTimeout, h)
	imgAPIRouter := imgAPI.Router()

	mainAPI := &api.API{
		Database:        db,
		Log:             log,
		Tracer:          tracer,
		RootURL:         *rootURL,
		ImageServiceURL: *rootURL,
		HandlerTimeout:  cmd.HandlerTimeout,
		HMAC:            h,
	}

	mainRouter, err := mainAPI.Router()
	if err != nil {
		log.Fatalf("error initializing router: %s", err)
	}

	mux := http.NewServeMux()
	uploadAPI := &uploadapi.API{
		DB:          db,
		StoragePath: *storagePath,
		SFTP:        sftpProvider,
	}

	mux.Handle("/health", handler.Health(checker))
	mux.Handle("/api/", uploadAPI.Router())
	mux.Handle("/admin", adminUI.Router())
	mux.Handle("/admin/", adminUI.Router())
	// Image processing routes — no auth required
	mux.HandleFunc("/id/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info") {
			mainRouter.ServeHTTP(w, r)
			return
		}
		imgAPIRouter.ServeHTTP(w, r)
	})
	// Redirect root to admin
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/admin", http.StatusFound)
			return
		}
		mainRouter.ServeHTTP(w, r)
	})

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

	log.Infof("picsum-photos listening on %s", listenAddr)

	go metrics.Serve(shutdownCtx, log, checker, *metricsListen)

	<-shutdownCtx.Done()
	log.Infof("shutting down: %s", shutdownCtx.Err())

	serverCtx, serverCancel := context.WithTimeout(ctx, cmd.WriteTimeout)
	defer serverCancel()
	if err := server.Shutdown(serverCtx); err != nil {
		log.Warnf("error shutting down: %s", err)
	}
}
