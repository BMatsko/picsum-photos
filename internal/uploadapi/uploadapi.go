// Package uploadapi provides a key-authenticated REST API for uploading images.
//
// Endpoints:
//
//	POST /api/v1/upload
//	  Authorization: Bearer pk_<key>
//
//	  Mode 1 — file upload (multipart/form-data):
//	    photo    file     JPEG or PNG file attachment
//	    author   string   required
//	    tags     string   comma-separated, optional
//	    notes    string   optional image notes
//	    alt_text string   optional alt text for accessibility
//	    id       string   optional — auto-incremented if omitted
//	    url      string   optional source label
//	    width    int      optional — auto-detected from image
//	    height   int      optional — auto-detected from image
//
//	  Mode 2 — URL fetch (application/json or multipart/form-data without photo):
//	    photo_url  string   public URL to fetch and re-host
//	    author     string   required
//	    tags       string   comma-separated, optional
//	    notes      string   optional image notes
//	    alt_text   string   optional alt text for accessibility
//	    id         string   optional
//	    url        string   optional override for source label (defaults to photo_url)
//
//	Returns 201 JSON: { "id": "...", "width": ..., "height": ..., "author": "...", "url": "...", "tags": [...], "notes": "...", "alt_text": "..." }
package uploadapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DMarby/picsum-photos/internal/database"
	"github.com/DMarby/picsum-photos/internal/database/postgres"
	imageformat "github.com/DMarby/picsum-photos/internal/storage/format"
	sftpStorage "github.com/DMarby/picsum-photos/internal/storage/sftp"
	"github.com/gorilla/mux"
)

// API handles key-authenticated upload requests.
type API struct {
	DB          *postgres.Provider
	StoragePath string
	SFTP        *sftpStorage.Provider // nil = local file storage
}

// Router returns the HTTP handler for the upload API.
func (a *API) Router() http.Handler {
	r := mux.NewRouter()
	r.HandleFunc("/api/v1/upload", a.apiKeyAuth(a.handleUpload)).Methods("POST")
	r.HandleFunc("/api/v1/images", a.apiKeyAuth(a.handleList)).Methods("GET")
	return r
}

// KeyAuthMiddleware wraps any http.Handler and requires a valid API key.
// The key can be supplied as:
//   - Authorization: Bearer pk_<key>  header
//   - ?key=pk_<key>                   query parameter
//
// /health is always exempt. Returns 401 JSON on failure.
func (a *API) KeyAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := ""
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			token = strings.TrimPrefix(auth, "Bearer ")
		} else if q := r.URL.Query().Get("key"); q != "" {
			token = q
		}
		if token == "" {
			jsonError(w, "API key required — pass ?key=<key> or Authorization: Bearer <key>", http.StatusUnauthorized)
			return
		}
		if _, err := a.DB.LookupAPIKey(r.Context(), token); err != nil {
			jsonError(w, "invalid API key", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiKeyAuth middleware — reads Bearer token from Authorization header.
func (a *API) apiKeyAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			jsonError(w, "missing or invalid Authorization header", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if _, err := a.DB.LookupAPIKey(r.Context(), token); err != nil {
			jsonError(w, "invalid API key", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleUpload dispatches to file-upload or URL-fetch mode.
func (a *API) handleUpload(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")

	// JSON body → URL mode
	if strings.HasPrefix(ct, "application/json") {
		a.handleUploadFromJSON(w, r)
		return
	}

	// Multipart — parse first, then decide based on whether photo_url is present
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		jsonError(w, "form parse error", http.StatusBadRequest)
		return
	}

	if r.FormValue("photo_url") != "" {
		a.handleUploadFromURL(w, r)
	} else {
		a.handleUploadFromFile(w, r)
	}
}

// ── Mode 1: file attachment ───────────────────────────────────────────────

func (a *API) handleUploadFromFile(w http.ResponseWriter, r *http.Request) {
	author := strings.TrimSpace(r.FormValue("author"))
	if author == "" {
		jsonError(w, "author is required", http.StatusBadRequest)
		return
	}

	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		nextID, err := a.DB.NextID(r.Context())
		if err != nil {
			jsonError(w, "failed to assign ID", http.StatusInternalServerError)
			return
		}
		id = fmt.Sprintf("%d", nextID)
	}

	file, header, err := r.FormFile("photo")
	if err != nil {
		jsonError(w, "photo file is required (or provide photo_url for URL mode)", http.StatusBadRequest)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		jsonError(w, "failed to read file", http.StatusInternalServerError)
		return
	}

	imgURL := strings.TrimSpace(r.FormValue("url"))
	if imgURL == "" {
		imgURL = header.Filename
	}

	fileExt := imageformat.DetectExtension(data)

	notes := strings.TrimSpace(r.FormValue("notes"))
	altText := strings.TrimSpace(r.FormValue("alt_text"))
	a.finishUpload(w, r, id, author, imgURL, header.Filename, fileExt, notes, altText, parseTags(r.FormValue("tags")), data)
}

// ── Mode 2: URL fetch ─────────────────────────────────────────────────────

type urlUploadRequest struct {
	PhotoURL string `json:"photo_url"`
	Author   string `json:"author"`
	Tags     string `json:"tags"`
	Notes    string `json:"notes"`
	AltText  string `json:"alt_text"`
	ID       string `json:"id"`
	URL      string `json:"url"`
}

// handleUploadFromJSON reads a JSON body and fetches the remote image.
func (a *API) handleUploadFromJSON(w http.ResponseWriter, r *http.Request) {
	var req urlUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	a.fetchAndStore(w, r, req.PhotoURL, req.Author, req.URL, req.Tags, req.Notes, req.AltText, req.ID)
}

// handleUploadFromURL reads photo_url from a multipart form.
func (a *API) handleUploadFromURL(w http.ResponseWriter, r *http.Request) {
	a.fetchAndStore(w, r,
		r.FormValue("photo_url"),
		r.FormValue("author"),
		r.FormValue("url"),
		r.FormValue("tags"),
		r.FormValue("notes"),
		r.FormValue("alt_text"),
		r.FormValue("id"),
	)
}

func (a *API) fetchAndStore(w http.ResponseWriter, r *http.Request, photoURL, author, srcURL, tagsRaw, notes, altText, id string) {
	author = strings.TrimSpace(author)
	if author == "" {
		jsonError(w, "author is required", http.StatusBadRequest)
		return
	}
	photoURL = strings.TrimSpace(photoURL)
	if photoURL == "" {
		jsonError(w, "photo_url is required", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(photoURL, "http://") && !strings.HasPrefix(photoURL, "https://") {
		jsonError(w, "photo_url must be an http/https URL", http.StatusBadRequest)
		return
	}

	// Fetch the remote image
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(photoURL)
	if err != nil {
		jsonError(w, "failed to fetch photo_url: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		jsonError(w, fmt.Sprintf("photo_url returned HTTP %d", resp.StatusCode), http.StatusBadGateway)
		return
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20)) // 100 MB limit
	if err != nil {
		jsonError(w, "failed to read remote image", http.StatusBadGateway)
		return
	}

	// Detect extension from Content-Type
	fileExt := ".jpg"
	remoteCT := resp.Header.Get("Content-Type")
	if strings.Contains(remoteCT, "image/png") {
		fileExt = ".png"
	}

	// Source URL label
	if srcURL == "" {
		srcURL = photoURL
	}

	// Auto-assign ID
	if id == "" {
		nextID, err := a.DB.NextID(r.Context())
		if err != nil {
			jsonError(w, "failed to assign ID", http.StatusInternalServerError)
			return
		}
		id = fmt.Sprintf("%d", nextID)
	}

	// Extract filename from the remote URL path
	fetchedFilename := photoURL
	if li := strings.LastIndex(photoURL, "/"); li >= 0 { fetchedFilename = photoURL[li+1:] }
	if qi := strings.Index(fetchedFilename, "?"); qi >= 0 { fetchedFilename = fetchedFilename[:qi] }
	a.finishUpload(w, r, id, author, srcURL, fetchedFilename, fileExt, notes, altText, parseTags(tagsRaw), data)
}

// ── Shared save + DB logic ────────────────────────────────────────────────

func (a *API) finishUpload(w http.ResponseWriter, r *http.Request, id, author, imgURL, filename, fileExt, notes, altText string, tags []string, data []byte) {
	id = normalizeImageID(id)
	// Detect dimensions
	var width, height int
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		width, height = cfg.Width, cfg.Height
	}

	// Save to storage
	if a.SFTP != nil {
		if err := a.SFTP.PutWithExt(id, fileExt, data); err != nil {
			jsonError(w, "failed to save file to storage", http.StatusInternalServerError)
			return
		}
	} else {
		destPath := filepath.Join(a.StoragePath, id+fileExt)
		if err := os.WriteFile(destPath, data, 0644); err != nil {
			jsonError(w, "failed to save file", http.StatusInternalServerError)
			return
		}
	}

	// Resolve tag aliases to canonical names before storing
	tags = a.DB.ResolveTags(r.Context(), tags)
	// Insert into DB
	_, dbErr := a.DB.Pool().Exec(r.Context(),
		`INSERT INTO images (id, author, url, filename, width, height, tags, notes, alt_text) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (id) DO UPDATE SET author=$2, url=$3, filename=$4, width=$5, height=$6, tags=$7, notes=$8, alt_text=$9`,
		id, author, imgURL, filename, width, height, tags, notes, altText,
	)
	if dbErr != nil {
		if a.SFTP != nil {
			_ = a.SFTP.Delete(id)
		} else {
			os.Remove(filepath.Join(a.StoragePath, id+fileExt))
		}
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{
		"id":       id,
		"author":   author,
		"url":      imgURL,
		"width":    width,
		"height":   height,
		"tags":     tags,
		"notes":    notes,
		"alt_text": altText,
	})
}

// handleList returns all images as JSON.
func normalizeImageID(id string) string {
	id = strings.TrimSpace(id)
	id = filepath.Base(id)
	lower := strings.ToLower(id)
	for _, ext := range imageformat.SupportedExtensions {
		if strings.HasSuffix(lower, ext) {
			return id[:len(id)-len(ext)]
		}
	}
	return id
}

func (a *API) handleList(w http.ResponseWriter, r *http.Request) {
	images, err := a.DB.ListAllWithTags(r.Context())
	if err != nil {
		jsonError(w, "database error", http.StatusInternalServerError)
		return
	}
	type entry struct {
		ID     string   `json:"id"`
		Author string   `json:"author"`
		URL    string   `json:"url"`
		Width  int      `json:"width"`
		Height int      `json:"height"`
		Tags     []string `json:"tags"`
		Notes    string   `json:"notes"`
		AltText  string   `json:"alt_text"`
	}
	out := make([]entry, len(images))
	for i, img := range images {
		out[i] = entry{
			ID: img.ID, Author: img.Author, URL: img.URL,
			Width: img.Width, Height: img.Height, Tags: img.Tags,
			Notes: img.Notes, AltText: img.AltText,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func parseTags(raw string) []string {
	var tags []string
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(strings.ToLower(t))
		if t != "" {
			tags = append(tags, t)
		}
	}
	return tags
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func init() {
	_ = database.ErrNotFound // ensure database import is used
}
