package imageapi

import (
	"expvar"
	"net/http"
	"sync"
	"time"

	"github.com/DMarby/picsum-photos/internal/handler"
	"github.com/DMarby/picsum-photos/internal/hmac"
	"github.com/DMarby/picsum-photos/internal/tracing"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/rs/cors"

	"github.com/DMarby/picsum-photos/internal/image"
	"github.com/DMarby/picsum-photos/internal/logger"
	"github.com/gorilla/mux"
)

const (
	imageCacheTTL      = 10 * time.Minute
	imageCacheCapacity = 150_000
)

// API is a http api
type API struct {
	ImageProcessor image.Processor
	Log            *logger.Logger
	Tracer         *tracing.Tracer
	HandlerTimeout time.Duration
	HMAC           *hmac.HMAC
	imageCache     *expirable.LRU[string, []byte] // caches processed images
	inflight       sync.Map                       // map[string]chan struct{} - coalesces concurrent requests
}

// NewAPI creates a new API instance with initialized caches
func NewAPI(imageProcessor image.Processor, log *logger.Logger, tracer *tracing.Tracer, handlerTimeout time.Duration, hmac *hmac.HMAC) *API {
	cache := expirable.NewLRU[string, []byte](imageCacheCapacity, nil, imageCacheTTL)

	// Publish cache size gauge metric (only if not already registered)
	if expvar.Get("gauge_imageapi_cache_size") == nil {
		expvar.Publish("gauge_imageapi_cache_size", expvar.Func(func() any {
			return cache.Len()
		}))
	}

	return &API{
		ImageProcessor: imageProcessor,
		Log:            log,
		Tracer:         tracer,
		HandlerTimeout: handlerTimeout,
		HMAC:           hmac,
		imageCache:     cache,
	}
}

// Utility methods for logging
func (a *API) logError(r *http.Request, message string, err error) {
	a.Log.Errorw(message, handler.LogFields(r, "error", err)...)
}

// Router returns a http router
func (a *API) Router() http.Handler {
	router := mux.NewRouter()

	router.NotFoundHandler = handler.Handler(a.notFoundHandler)

	// Redirect trailing slashes
	router.StrictSlash(true)

	// Image by ID routes
	router.Handle("/id/{id}/{width:[0-9]+}/{height:[0-9]+}{extension:(?:\\..*)?}", handler.Handler(a.imageHandler)).Methods("GET").Name("imageapi.image")

	// Query parameters:
	// ?grayscale - Grayscale the image
	// ?blur={amount} - Blur the image by {amount}

	// ?hmac - HMAC signature of the path and URL parameters

	// Set up handlers
	cors := cors.New(cors.Options{
		AllowedMethods: []string{"GET"},
		AllowedOrigins: []string{"*"},
		ExposedHeaders: []string{"Content-Type", "Picsum-ID"},
	})

	httpHandler := cors.Handler(router)
	httpHandler = handler.Recovery(a.Log, httpHandler)
	httpHandler = http.TimeoutHandler(httpHandler, a.HandlerTimeout, "Something went wrong. Timed out.")
	httpHandler = handler.Logger(a.Log, httpHandler)

	routeMatcher := &handler.MuxRouteMatcher{Router: router}
	httpHandler = handler.Tracer(a.Tracer, httpHandler, routeMatcher)
	httpHandler = handler.Metrics(httpHandler, routeMatcher)

	return httpHandler
}

// Handle not found errors
var notFoundError = &handler.Error{
	Message: "page not found",
	Code:    http.StatusNotFound,
}

func (a *API) notFoundHandler(w http.ResponseWriter, r *http.Request) *handler.Error {
	return notFoundError
}
