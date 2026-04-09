// Package uploadapi provides a key-authenticated REST API for uploading images.
//
// Endpoints:
//
//	POST /api/v1/upload
//	  Authorization: Bearer pk_<key>
//	  Content-Type: multipart/form-data
//	  Fields: photo (file), author (string), tags (comma-separated, optional),
//	          id (string, optional — auto-assigned if omitted),
//	          width/height (int, optional — auto-detected from JPEG)
//
//	Returns 201 JSON: { "id": "...", "width": ..., "height": ..., "author": "...", "tags": [...] }
package uploadapi

import (
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/DMarby/picsum-photos/internal/database"
	"github.com/DMarby/picsum-photos/internal/database/postgres"
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

// handleUpload processes a multipart upload and saves the image.
func (a *API) handleUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		jsonError(w, "form parse error", http.StatusBadRequest)
		return
	}

	author := strings.TrimSpace(r.FormValue("author"))
	if author == "" {
		jsonError(w, "author is required", http.StatusBadRequest)
		return
	}

	// ID: use provided or auto-assign
	id := strings.TrimSpace(r.FormValue("id"))
	if id == "" {
		nextID, err := a.DB.NextID(r.Context())
		if err != nil {
			jsonError(w, "failed to assign ID", http.StatusInternalServerError)
			return
		}
		id = fmt.Sprintf("%d", nextID)
	}

	// Read the uploaded file
	file, header, err := r.FormFile("photo")
	if err != nil {
		jsonError(w, "photo file is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Read file bytes
	data := make([]byte, header.Size)
	if _, err := file.Read(data); err != nil {
		jsonError(w, "failed to read file", http.StatusInternalServerError)
		return
	}

	// Detect dimensions from JPEG if not provided
	var width, height int
	fmt.Sscan(r.FormValue("width"), &width)
	fmt.Sscan(r.FormValue("height"), &height)
	if width == 0 || height == 0 {
		if cfg, _, err := image.DecodeConfig(strings.NewReader(string(data))); err == nil {
			width, height = cfg.Width, cfg.Height
		}
	}

	// Source URL
	imgURL := strings.TrimSpace(r.FormValue("url"))
	if imgURL == "" {
		imgURL = header.Filename
	}

	// Tags
	var tags []string
	for _, t := range strings.Split(r.FormValue("tags"), ",") {
		t = strings.TrimSpace(strings.ToLower(t))
		if t != "" {
			tags = append(tags, t)
		}
	}

	// Determine extension from content type
	fileExt := ".jpg"
	if header.Header.Get("Content-Type") == "image/png" {
		fileExt = ".png"
	}

	// Save file
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

	// Insert into DB
	_, dbErr := a.DB.Pool().Exec(r.Context(),
		`INSERT INTO images (id, author, url, width, height, tags) VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (id) DO UPDATE SET author=$2, url=$3, width=$4, height=$5, tags=$6`,
		id, author, imgURL, width, height, tags,
	)
	if dbErr != nil {
		// Roll back file write
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
		"id":     id,
		"author": author,
		"url":    imgURL,
		"width":  width,
		"height": height,
		"tags":   tags,
	})
}

// handleList returns all images as JSON (same as /v2/list but auth-gated).
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
		Tags   []string `json:"tags"`
	}
	out := make([]entry, len(images))
	for i, img := range images {
		out[i] = entry{
			ID: img.ID, Author: img.Author, URL: img.URL,
			Width: img.Width, Height: img.Height, Tags: img.Tags,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// imageDecodeConfig helper — image.DecodeConfig needs an io.Reader not a string
// so we use a bytes.Reader instead.
func init() {
	// Ensure jpeg decoder is registered (already done via _ "image/jpeg" import)
	_ = database.ErrNotFound // silence unused import check
}
