package imageapi

import (
	"errors"
	"expvar"
	"fmt"
	"net/http"
	"strconv"

	"github.com/DMarby/picsum-photos/internal/handler"
	"github.com/DMarby/picsum-photos/internal/image"
	"github.com/DMarby/picsum-photos/internal/params"
	"github.com/DMarby/picsum-photos/internal/queue"
	"github.com/gorilla/mux"
)

// Metrics for cache and request coalescing
var (
	cacheHits         = expvar.NewInt("counter_imageapi_cache_hits")
	cacheMisses       = expvar.NewInt("counter_imageapi_cache_misses")
	requestsCoalesced = expvar.NewInt("counter_imageapi_requests_coalesced")
	requestsProcessed = expvar.NewInt("counter_imageapi_requests_processed")
	queueFullErrors   = expvar.NewInt("counter_imageapi_queue_full_errors")
)

func (a *API) imageHandler(w http.ResponseWriter, r *http.Request) *handler.Error {
	// Get the path and query parameters
	p, err := params.GetParams(r)
	if err != nil {
		return handler.BadRequest(err.Error())
	}

	// Get the image ID from the path param
	vars := mux.Vars(r)
	imageID := vars["id"]

	// Build the cache key for request coalescing
	cacheKey := buildCacheKey(imageID, p)

	// Request coalescing with LRU cache pattern
	// This prevents the "thundering herd" problem where many identical
	// requests arrive simultaneously and all hit the image processor

	// First, check the LRU cache for a cached result
	if cachedImage, ok := a.imageCache.Get(cacheKey); ok {
		cacheHits.Add(1)
		return a.sendImage(w, imageID, p, cachedImage)
	}
	cacheMisses.Add(1)

	// Cache miss - use request coalescing to prevent duplicate processing
	// Create a channel to signal when processing is complete
	done := make(chan struct{})

	// Try to claim responsibility for this request
	existing, loaded := a.inflight.LoadOrStore(cacheKey, done)
	if loaded {
		// Another goroutine is already processing this request, wait for it
		requestsCoalesced.Add(1)
		select {
		case <-existing.(chan struct{}):
			// Processing complete, result should now be in cache
			if cachedImage, ok := a.imageCache.Get(cacheKey); ok {
				return a.sendImage(w, imageID, p, cachedImage)
			}
			// Cache miss after waiting (possibly evicted or error occurred)
			// Fall through to process the image ourselves
		case <-r.Context().Done():
			// Request was cancelled
			return handler.InternalServerError()
		}
	}

	// We're responsible for processing this request (or retry after cache miss)
	requestsProcessed.Add(1)

	// Build the image task
	task := image.NewTask(imageID, p.Width, p.Height, fmt.Sprintf("Picsum ID: %s", imageID), getOutputFormat(p.Extension))
	if p.Blur {
		task.Blur(p.BlurAmount)
	}

	if p.Grayscale {
		task.Grayscale()
	}

	// Process the image
	processedImage, err := a.ImageProcessor.ProcessImage(r.Context(), task)

	// Cleanup and signal completion
	if !loaded {
		a.inflight.Delete(cacheKey)
		close(done)
	}

	if err != nil {
		if errors.Is(err, queue.ErrQueueFull) {
			queueFullErrors.Add(1)
			a.logError(r, "error processing image: queue is full", err)
			return handler.ServiceUnavailable()
		}
		a.logError(r, "error processing image", err)
		return handler.InternalServerError()
	}

	// Store in LRU cache for future requests
	a.imageCache.Add(cacheKey, processedImage)

	return a.sendImage(w, imageID, p, processedImage)
}

// sendImage writes the processed image to the response with appropriate headers
func (a *API) sendImage(w http.ResponseWriter, imageID string, p *params.Params, processedImage []byte) *handler.Error {
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", buildFilename(imageID, p)))
	w.Header().Set("Content-Type", getContentType(p.Extension))
	w.Header().Set("Content-Length", strconv.Itoa(len(processedImage)))
	w.Header().Set("Cache-Control", "public, max-age=2592000, stale-while-revalidate=60, stale-if-error=43200, immutable") // Cache for a month
	w.Header().Set("Picsum-ID", imageID)
	w.Header().Set("Timing-Allow-Origin", "*") // Allow all origins to see timing resources

	w.Write(processedImage)

	return nil
}

func getOutputFormat(extension string) image.OutputFormat {
	switch extension {
	case ".webp":
		return image.WebP
	default:
		return image.JPEG
	}
}

func getContentType(extension string) string {
	switch extension {
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

// buildCacheKey creates a unique key for request coalescing based on image parameters
func buildCacheKey(imageID string, p *params.Params) string {
	key := fmt.Sprintf("%s-%dx%d%s", imageID, p.Width, p.Height, p.Extension)

	if p.Blur {
		key += fmt.Sprintf("-blur_%d", p.BlurAmount)
	}

	if p.Grayscale {
		key += "-grayscale"
	}

	return key
}

func buildFilename(imageID string, p *params.Params) string {
	filename := fmt.Sprintf("%s-%dx%d", imageID, p.Width, p.Height)

	if p.Blur {
		filename += fmt.Sprintf("-blur_%d", p.BlurAmount)
	}

	if p.Grayscale {
		filename += "-grayscale"
	}

	filename += p.Extension

	return filename
}
