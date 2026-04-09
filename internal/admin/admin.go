package admin

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	_ "image/jpeg"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DMarby/picsum-photos/internal/database/postgres"
	sftpStorage "github.com/DMarby/picsum-photos/internal/storage/sftp"
	"github.com/gorilla/mux"
)

//go:embed templates/*
var templateFS embed.FS

// Admin holds dependencies for the admin UI
type Admin struct {
	DB           *postgres.Provider
	StoragePath  string
	SFTP         *sftpStorage.Provider // nil when using local file storage
	Password     string
	RootURL      string
	templates    *template.Template
}

var sessionToken = fmt.Sprintf("sess_%d", time.Now().UnixNano())

// New creates an Admin instance and parses templates.
func New(db *postgres.Provider, storagePath string, sftp *sftpStorage.Provider, password, rootURL string) (*Admin, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"map": func(kv ...any) map[string]any {
			m := make(map[string]any)
			for i := 0; i+1 < len(kv); i += 2 {
				m[fmt.Sprint(kv[i])] = kv[i+1]
			}
			return m
		},
		"not": func(v any) bool {
			if v == nil {
				return true
			}
			switch t := v.(type) {
			case bool:
				return !t
			case int:
				return t == 0
			case string:
				return t == ""
			default:
				return false
			}
		},
		"join": strings.Join,
	}).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parsing admin templates: %w", err)
	}
	return &Admin{
		DB:          db,
		StoragePath: storagePath,
		SFTP:        sftp,
		Password:    password,
		RootURL:     rootURL,
		templates:   tmpl,
	}, nil
}

// Router returns the admin HTTP handler.
func (a *Admin) Router() http.Handler {
	r := mux.NewRouter()
	r.HandleFunc("/admin/login", a.handleLoginPage).Methods("GET")
	r.HandleFunc("/admin/login", a.handleLoginSubmit).Methods("POST")
	r.HandleFunc("/admin/logout", a.handleLogout).Methods("POST")
	r.HandleFunc("/admin", a.auth(a.handleDashboard)).Methods("GET")
	r.HandleFunc("/admin/photos", a.auth(a.handlePhotos)).Methods("GET")
	r.HandleFunc("/admin/photos/upload", a.auth(a.handleUpload)).Methods("POST")
	r.HandleFunc("/admin/photos/{id}/delete", a.auth(a.handleDelete)).Methods("POST")
	r.HandleFunc("/admin/photos/{id}/tags", a.auth(a.handleUpdateTags)).Methods("POST")
	r.HandleFunc("/admin/seeds", a.auth(a.handleSeeds)).Methods("GET")
	r.HandleFunc("/admin/seeds/clear", a.auth(a.handleClearSeed)).Methods("POST")
	r.HandleFunc("/admin/docs", a.auth(a.handleDocs)).Methods("GET")
	r.HandleFunc("/admin/apikeys", a.auth(a.handleAPIKeys)).Methods("GET")
	r.HandleFunc("/admin/apikeys/create", a.auth(a.handleCreateAPIKey)).Methods("POST")
	r.HandleFunc("/admin/apikeys/{id}/revoke", a.auth(a.handleRevokeAPIKey)).Methods("POST")
	// JSON APIs
	r.HandleFunc("/admin/api/next-id", a.auth(a.handleNextID)).Methods("GET")
	r.HandleFunc("/admin/api/images", a.auth(a.handleImageList)).Methods("GET")
	return r
}

// ── Auth ──────────────────────────────────────────────────────────────────────

func (a *Admin) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("admin_session")
		if err != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(sessionToken)) != 1 {
			http.Redirect(w, r, "/admin/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

// ── Handlers ──────────────────────────────────────────────────────────────────

func (a *Admin) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, "login.html", map[string]any{"Error": r.URL.Query().Get("error")})
}

func (a *Admin) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(a.Password)) != 1 {
		http.Redirect(w, r, "/admin/login?error=Invalid+password", http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "admin_session", Value: sessionToken,
		Path: "/admin", HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/admin", http.StatusFound)
}

func (a *Admin) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "admin_session", Value: "", Path: "/admin", MaxAge: -1})
	http.Redirect(w, r, "/admin/login", http.StatusFound)
}

func (a *Admin) handleDashboard(w http.ResponseWriter, r *http.Request) {
	images, _ := a.DB.ListAll(r.Context())
	a.render(w, "dashboard.html", map[string]any{
		"Page": "dashboard", "PhotoCount": len(images), "RootURL": a.RootURL,
	})
}

func (a *Admin) handlePhotos(w http.ResponseWriter, r *http.Request) {
	images, err := a.DB.ListAllWithTags(r.Context())
	if err != nil {
		http.Error(w, "Database error", 500)
		return
	}
	a.render(w, "photos.html", map[string]any{
		"Page": "photos", "Images": images, "RootURL": a.RootURL,
		"Success": r.URL.Query().Get("success"),
		"Error":   r.URL.Query().Get("error"),
	})
}

func (a *Admin) handleUpload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(100 << 20); err != nil {
		http.Redirect(w, r, "/admin/photos?error=Form+parse+error", http.StatusFound)
		return
	}

	id := strings.TrimSpace(r.FormValue("id"))
	author := strings.TrimSpace(r.FormValue("author"))
	imgURL := strings.TrimSpace(r.FormValue("url"))
	tagsRaw := strings.TrimSpace(r.FormValue("tags"))

	var width, height int
	fmt.Sscan(r.FormValue("width"), &width)
	fmt.Sscan(r.FormValue("height"), &height)

	if id == "" || author == "" {
		http.Redirect(w, r, "/admin/photos?error=ID+and+Author+are+required", http.StatusFound)
		return
	}

	// Parse tags
	var tags []string
	for _, t := range strings.Split(tagsRaw, ",") {
		t = strings.TrimSpace(strings.ToLower(t))
		if t != "" {
			tags = append(tags, t)
		}
	}

	file, header, err := r.FormFile("photo")
	if err != nil {
		http.Redirect(w, r, "/admin/photos?error=No+photo+file+provided", http.StatusFound)
		return
	}
	defer file.Close()

	// Read file into memory to decode dimensions if not provided
	data, err := io.ReadAll(file)
	if err != nil {
		http.Redirect(w, r, "/admin/photos?error=Failed+to+read+file", http.StatusFound)
		return
	}

	if width == 0 || height == 0 {
		if cfg, _, err2 := image.DecodeConfig(strings.NewReader(string(data))); err2 == nil {
			width, height = cfg.Width, cfg.Height
		}
	}

	// Use filename as source URL hint if not provided
	if imgURL == "" && header != nil {
		imgURL = header.Filename
	}

	// Determine extension from MIME type
	fileExt := ".jpg"
	if header != nil {
		mt := header.Header.Get("Content-Type")
		if mt == "image/png" {
			fileExt = ".png"
		}
	}

	// Write file to storage (SFTP or local)
	if a.SFTP != nil {
		if err := a.SFTP.PutWithExt(id, fileExt, data); err != nil {
			http.Redirect(w, r, "/admin/photos?error=Failed+to+save+file+to+SFTP", http.StatusFound)
			return
		}
	} else {
		destPath := filepath.Join(a.StoragePath, id+fileExt)
		if err := os.WriteFile(destPath, data, 0644); err != nil {
			http.Redirect(w, r, "/admin/photos?error=Failed+to+save+file", http.StatusFound)
			return
		}
	}

	_, err = a.DB.Pool().Exec(r.Context(),
		`INSERT INTO images (id, author, url, width, height, tags) VALUES ($1,$2,$3,$4,$5,$6)
		 ON CONFLICT (id) DO UPDATE SET author=$2, url=$3, width=$4, height=$5, tags=$6`,
		id, author, imgURL, width, height, tags,
	)
	if err != nil {
		if a.SFTP != nil {
			_ = a.SFTP.Delete(id)
		} else {
			os.Remove(filepath.Join(a.StoragePath, id+fileExt))
		}
		http.Redirect(w, r, "/admin/photos?error=Database+error", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/photos?success=Photo+uploaded", http.StatusFound)
}

func (a *Admin) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if _, err := a.DB.Pool().Exec(r.Context(), `DELETE FROM images WHERE id = $1`, id); err != nil {
		http.Redirect(w, r, "/admin/photos?error=Delete+failed", http.StatusFound)
		return
	}
	if a.SFTP != nil {
		_ = a.SFTP.Delete(id)
	} else {
		os.Remove(filepath.Join(a.StoragePath, id+".jpg"))
	}
	http.Redirect(w, r, "/admin/photos?success=Photo+deleted", http.StatusFound)
}

func (a *Admin) handleUpdateTags(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	r.ParseForm()
	tagsRaw := strings.TrimSpace(r.FormValue("tags"))

	var tags []string
	for _, t := range strings.Split(tagsRaw, ",") {
		t = strings.TrimSpace(strings.ToLower(t))
		if t != "" {
			tags = append(tags, t)
		}
	}

	if _, err := a.DB.Pool().Exec(r.Context(),
		`UPDATE images SET tags = $1 WHERE id = $2`, tags, id); err != nil {
		http.Redirect(w, r, "/admin/photos?error=Tag+update+failed", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/photos?success=Tags+updated", http.StatusFound)
}

func (a *Admin) handleSeeds(w http.ResponseWriter, r *http.Request) {
	type SeedRow struct {
		Seed      string
		ImageID   string
		Author    string
		CreatedAt string
	}
	rows, err := a.DB.Pool().Query(r.Context(),
		`SELECT sr.seed, sr.image_id, i.author, sr.created_at
		 FROM seed_resolutions sr JOIN images i ON i.id = sr.image_id
		 ORDER BY sr.created_at DESC LIMIT 200`)
	var seeds []SeedRow
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var s SeedRow
			var ts time.Time
			rows.Scan(&s.Seed, &s.ImageID, &s.Author, &ts)
			s.CreatedAt = ts.Format("Jan 2, 2006 15:04")
			seeds = append(seeds, s)
		}
	}

	images, _ := a.DB.ListAllWithTags(r.Context())
	a.render(w, "seeds.html", map[string]any{
		"Page": "seeds", "Seeds": seeds, "Images": images, "RootURL": a.RootURL,
	})
}

func (a *Admin) handleClearSeed(w http.ResponseWriter, r *http.Request) {
	seed := r.FormValue("seed")
	if seed == "" {
		// Clear all
		a.DB.Pool().Exec(r.Context(), `DELETE FROM seed_resolutions`)
	} else {
		a.DB.Pool().Exec(r.Context(), `DELETE FROM seed_resolutions WHERE seed = $1`, seed)
	}
	http.Redirect(w, r, "/admin/seeds?success=Seed+cleared", http.StatusFound)
}

func (a *Admin) handleDocs(w http.ResponseWriter, r *http.Request) {
	a.render(w, "docs.html", map[string]any{"Page": "docs", "RootURL": a.RootURL})
}

func (a *Admin) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := a.DB.ListAPIKeys(r.Context())
	if err != nil {
		http.Error(w, "Database error", 500)
		return
	}
	a.render(w, "apikeys.html", map[string]any{
		"Page": "apikeys", "Keys": keys, "RootURL": a.RootURL,
		"Success": r.URL.Query().Get("success"),
		"Error":   r.URL.Query().Get("error"),
		"NewKey":  r.URL.Query().Get("key"),
	})
}

func (a *Admin) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/admin/apikeys?error=Name+is+required", http.StatusFound)
		return
	}
	_, plaintext, err := a.DB.CreateAPIKey(r.Context(), name)
	if err != nil {
		http.Redirect(w, r, "/admin/apikeys?error=Failed+to+create+key", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/apikeys?key="+plaintext, http.StatusFound)
}

func (a *Admin) handleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if err := a.DB.DeleteAPIKey(r.Context(), id); err != nil {
		http.Redirect(w, r, "/admin/apikeys?error=Failed+to+revoke+key", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/apikeys?success=Key+revoked", http.StatusFound)
}

func (a *Admin) handleNextID(w http.ResponseWriter, r *http.Request) {
	nextID, _ := a.DB.NextID(r.Context())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"next_id": nextID})
}

// handleImageList returns a lightweight JSON list of all images for client-side duplicate detection.
func (a *Admin) handleImageList(w http.ResponseWriter, r *http.Request) {
	images, err := a.DB.ListAllWithTags(r.Context())
	if err != nil {
		http.Error(w, "db error", 500)
		return
	}
	type entry struct {
		ID     string `json:"id"`
		Author string `json:"author"`
		URL    string `json:"url"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	}
	out := make([]entry, len(images))
	for i, img := range images {
		out[i] = entry{ID: img.ID, Author: img.Author, URL: img.URL, Width: img.Width, Height: img.Height}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (a *Admin) render(w http.ResponseWriter, tmpl string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, tmpl, data); err != nil {
		http.Error(w, "Template error: "+err.Error(), 500)
	}
}
