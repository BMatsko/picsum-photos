package admin

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	"io"
	"net/http"
	"net/url"
	"sort"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/DMarby/picsum-photos/internal/database/postgres"
	imageformat "github.com/DMarby/picsum-photos/internal/storage/format"
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
		"percent": func(val, max int) int {
			if max == 0 { return 0 }
			v := val * 100 / max
			if v < 2 && val > 0 { return 2 }
			return v
		},
		"taggedCount": func(total, untagged int) int { return total - untagged },
		"taggedPct": func(total, untagged int) int {
			if total == 0 { return 0 }
			return (total - untagged) * 100 / total
		},
		"seedPct": func(tagged, total int) int {
			if total == 0 { return 0 }
			return tagged * 100 / total
		},
		"seedUntagged": func(total, tagged int) int { return total - tagged },
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
	r.HandleFunc("/admin/photos/{id}/metadata", a.auth(a.handleUpdateMetadata)).Methods("POST")
	r.HandleFunc("/admin/seeds", a.auth(a.handleSeeds)).Methods("GET")
	r.HandleFunc("/admin/seeds/clear", a.auth(a.handleClearSeed)).Methods("POST")
	r.HandleFunc("/admin/seeds/clear-by-tag", a.auth(a.handleClearSeedsByTag)).Methods("POST")
	r.HandleFunc("/admin/seeds/clear-untagged", a.auth(a.handleClearUntaggedSeeds)).Methods("POST")
	r.HandleFunc("/admin/docs", a.auth(a.handleDocs)).Methods("GET")
	r.HandleFunc("/admin/apikeys", a.auth(a.handleAPIKeys)).Methods("GET")
	r.HandleFunc("/admin/apikeys/create", a.auth(a.handleCreateAPIKey)).Methods("POST")
	r.HandleFunc("/admin/apikeys/{id}/revoke", a.auth(a.handleRevokeAPIKey)).Methods("POST")
	r.HandleFunc("/admin/dedupe", a.auth(a.handleDedupe)).Methods("GET")
	r.HandleFunc("/admin/dedupe/merge", a.auth(a.handleMerge)).Methods("POST")
	r.HandleFunc("/admin/api/images-for-dedupe", a.auth(a.handleImagesForDedupe)).Methods("GET")
	r.HandleFunc("/admin/tags", a.auth(a.handleTags)).Methods("GET")
	r.HandleFunc("/admin/tags/create", a.auth(a.handleCreateTag)).Methods("POST")
	r.HandleFunc("/admin/tags/{id}/update", a.auth(a.handleUpdateTagEntry)).Methods("POST")
	r.HandleFunc("/admin/tags/{id}/delete", a.auth(a.handleDeleteTag)).Methods("POST")
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
	images, _ := a.DB.ListAllWithTags(r.Context())

	// Aggregations
	authorCounts := map[string]int{}
	tagCounts := map[string]int{}
	untagged := 0
	totalW, totalH := int64(0), int64(0)

	for _, img := range images {
		if img.Author != "" {
			authorCounts[img.Author]++
		}
		if len(img.Tags) == 0 {
			untagged++
		} else {
			for _, t := range img.Tags {
				tagCounts[t]++
			}
		}
		totalW += int64(img.Width)
		totalH += int64(img.Height)
	}

	// Sort authors by count descending (top 10)
	type kv struct{ K string; V int }
	var authorSlice []kv
	for k, v := range authorCounts {
		authorSlice = append(authorSlice, kv{k, v})
	}
	sort.Slice(authorSlice, func(i, j int) bool { return authorSlice[i].V > authorSlice[j].V })
	if len(authorSlice) > 10 {
		authorSlice = authorSlice[:10]
	}

	// Sort tags by count descending (top 10)
	var tagSlice []kv
	for k, v := range tagCounts {
		tagSlice = append(tagSlice, kv{k, v})
	}
	sort.Slice(tagSlice, func(i, j int) bool { return tagSlice[i].V > tagSlice[j].V })
	if len(tagSlice) > 10 {
		tagSlice = tagSlice[:10]
	}

	// Seed stats
	var seedTotal, seedTagged int
	a.DB.Pool().QueryRow(r.Context(), `SELECT COUNT(*) FROM seed_resolutions`).Scan(&seedTotal)
	a.DB.Pool().QueryRow(r.Context(), `SELECT COUNT(*) FROM seed_resolutions WHERE tag != ''`).Scan(&seedTagged)

	// Average dimensions
	avgW, avgH := 0, 0
	if len(images) > 0 {
		avgW = int(totalW / int64(len(images)))
		avgH = int(totalH / int64(len(images)))
	}

	a.render(w, "dashboard.html", map[string]any{
		"Page": "dashboard", "RootURL": a.RootURL,
		"PhotoCount": len(images),
		"Untagged":   untagged,
		"SeedTotal":  seedTotal,
		"SeedTagged": seedTagged,
		"AvgW": avgW, "AvgH": avgH,
		"AuthorStats": authorSlice,
		"TagStats":    tagSlice,
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
	notes := strings.TrimSpace(r.FormValue("notes"))
	altText := strings.TrimSpace(r.FormValue("alt_text"))
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
	// Store original filename separately from source URL
	filename := ""
	if header != nil {
		filename = header.Filename
	}
	if imgURL == "" && filename != "" {
		imgURL = filename
	}

	// Detect extension from magic bytes (more reliable than Content-Type)
	fileExt := imageformat.DetectExtension(data)

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

	// Resolve tag aliases to canonical names before storing
	tags = a.DB.ResolveTags(r.Context(), tags)
	_, err = a.DB.Pool().Exec(r.Context(),
		`INSERT INTO images (id, author, url, filename, width, height, tags, notes, alt_text) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		 ON CONFLICT (id) DO UPDATE SET author=$2, url=$3, filename=$4, width=$5, height=$6, tags=$7, notes=$8, alt_text=$9`,
		id, author, imgURL, filename, width, height, tags, notes, altText,
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
	id := normalizeImageID(mux.Vars(r)["id"])
	if _, err := a.DB.Pool().Exec(r.Context(), `DELETE FROM images WHERE id = $1`, id); err != nil {
		http.Redirect(w, r, "/admin/photos?error=Delete+failed", http.StatusFound)
		return
	}
	if a.SFTP != nil {
		_ = a.SFTP.Delete(id)
	} else {
		for _, ext := range imageformat.SupportedExtensions {
			os.Remove(filepath.Join(a.StoragePath, id+ext))
		}
	}
	http.Redirect(w, r, "/admin/photos?success=Photo+deleted", http.StatusFound)
}

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
	// Resolve aliases to canonical tag names
	tags = a.DB.ResolveTags(r.Context(), tags)

	if _, err := a.DB.Pool().Exec(r.Context(),
		`UPDATE images SET tags = $1 WHERE id = $2`, tags, id); err != nil {
		http.Redirect(w, r, "/admin/photos?error=Tag+update+failed", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/photos?success=Tags+updated", http.StatusFound)
}

func (a *Admin) handleUpdateMetadata(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	r.ParseForm()
	notes := strings.TrimSpace(r.FormValue("notes"))
	altText := strings.TrimSpace(r.FormValue("alt_text"))

	if _, err := a.DB.Pool().Exec(r.Context(),
		`UPDATE images SET notes = $1, alt_text = $2 WHERE id = $3`, notes, altText, id); err != nil {
		http.Redirect(w, r, "/admin/photos?error=Metadata+update+failed", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/photos?success=Metadata+updated", http.StatusFound)
}

func (a *Admin) handleSeeds(w http.ResponseWriter, r *http.Request) {
	type TagVariant struct {
		Tag       string
		ImageID   string
		Author    string
		CreatedAt string
		Deleted   bool
	}
	type SeedGroup struct {
		Seed     string
		Variants []TagVariant
	}

	rows, err := a.DB.Pool().Query(r.Context(),
		`SELECT sr.seed, COALESCE(sr.tag,''), COALESCE(sr.image_id,''), COALESCE(i.author,''), sr.image_id IS NULL AND EXISTS(SELECT 1 FROM seed_resolutions sr2 WHERE sr2.seed=sr.seed AND sr2.tag=sr.tag) as deleted, sr.created_at
		 FROM seed_resolutions sr LEFT JOIN images i ON i.id = sr.image_id
		 ORDER BY sr.seed, sr.tag`)

	seedMap := map[string]*SeedGroup{}
	var seedOrder []string
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var seed, tag, imageID, author string
			var deleted bool
			var ts time.Time
			rows.Scan(&seed, &tag, &imageID, &author, &deleted, &ts)
			if author == "" && imageID != "" {
				author = "(deleted)"
			}
			if _, exists := seedMap[seed]; !exists {
				seedMap[seed] = &SeedGroup{Seed: seed}
				seedOrder = append(seedOrder, seed)
			}
			seedMap[seed].Variants = append(seedMap[seed].Variants, TagVariant{
				Tag: tag, ImageID: imageID, Author: author,
				Deleted: deleted || imageID == "",
				CreatedAt: ts.Format("Jan 2, 2006 15:04"),
			})
		}
	}

	var groups []SeedGroup
	for _, s := range seedOrder {
		groups = append(groups, *seedMap[s])
	}

	a.render(w, "seeds.html", map[string]any{
		"Page": "seeds", "Groups": groups, "RootURL": a.RootURL,
		"Success": r.URL.Query().Get("success"),
		"Error":   r.URL.Query().Get("error"),
	})
}

func (a *Admin) handleClearSeedsByTag(w http.ResponseWriter, r *http.Request) {
	tag := strings.TrimSpace(strings.ToLower(r.FormValue("tag")))
	if tag == "" {
		http.Redirect(w, r, "/admin/seeds?error=Tag+is+required", http.StatusFound)
		return
	}
	result, err := a.DB.Pool().Exec(r.Context(),
		`DELETE FROM seed_resolutions WHERE tag = $1`, tag)
	if err != nil {
		http.Redirect(w, r, "/admin/seeds?error=Failed+to+clear+seeds", http.StatusFound)
		return
	}
	n := result.RowsAffected()
	http.Redirect(w, r, fmt.Sprintf("/admin/seeds?success=Cleared+%d+seed%%28s%%29+with+tag+%%22%s%%22", n, tag), http.StatusFound)
}

func (a *Admin) handleClearUntaggedSeeds(w http.ResponseWriter, r *http.Request) {
	result, err := a.DB.Pool().Exec(r.Context(),
		`DELETE FROM seed_resolutions WHERE tag = ''`)
	if err != nil {
		http.Redirect(w, r, "/admin/seeds?error=Failed+to+clear+untagged+seeds", http.StatusFound)
		return
	}
	n := result.RowsAffected()
	http.Redirect(w, r, fmt.Sprintf("/admin/seeds?success=Cleared+%d+untagged+seed%%28s%%29", n), http.StatusFound)
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

// ── Deduplication handlers ─────────────────────────────────────────────────────

func (a *Admin) handleDedupe(w http.ResponseWriter, r *http.Request) {
	a.render(w, "dedupe.html", map[string]any{
		"Page": "dedupe", "RootURL": a.RootURL,
		"Success": r.URL.Query().Get("success"),
		"Error":   r.URL.Query().Get("error"),
	})
}

// handleImagesForDedupe returns a lightweight list of all images for client-side hash scanning.
func (a *Admin) handleImagesForDedupe(w http.ResponseWriter, r *http.Request) {
	images, err := a.DB.ListAllWithTags(r.Context())
	if err != nil {
		jsonError(w, "db error", 500)
		return
	}
	type entry struct {
		ID     string   `json:"id"`
		Author string   `json:"author"`
		Width  int      `json:"width"`
		Height int      `json:"height"`
		Tags   []string `json:"tags"`
		URL    string   `json:"url"`
	}
	out := make([]entry, len(images))
	for i, img := range images {
		tags := img.Tags
		if tags == nil {
			tags = []string{}
		}
		out[i] = entry{ID: img.ID, Author: img.Author, Width: img.Width, Height: img.Height, Tags: tags, URL: img.URL}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// handleMerge merges the loser image into the winner:
// - union of tags written to winner
// - all seed_resolutions pointing to loser rewritten to winner
// - loser DB row deleted (cascades seed_resolutions)
// - loser file deleted from storage
// Returns 204 No Content on success for the AJAX merge flow.
func (a *Admin) handleMerge(w http.ResponseWriter, r *http.Request) {
	var winnerID string
	var loserIDs []string

	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		var payload struct {
			Winner string   `json:"winner"`
			Loser  string   `json:"loser"`
			Losers []string `json:"losers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		winnerID = strings.TrimSpace(payload.Winner)
		if len(payload.Losers) > 0 {
			loserIDs = append(loserIDs, payload.Losers...)
		} else if payload.Loser != "" {
			loserIDs = append(loserIDs, payload.Loser)
		}
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		winnerID = strings.TrimSpace(r.FormValue("winner"))
		if raw := strings.TrimSpace(r.FormValue("losers")); raw != "" {
			for _, part := range strings.Split(raw, ",") {
				if v := strings.TrimSpace(part); v != "" {
					loserIDs = append(loserIDs, v)
				}
			}
		} else if loser := strings.TrimSpace(r.FormValue("loser")); loser != "" {
			loserIDs = append(loserIDs, loser)
		}
	default:
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		winnerID = strings.TrimSpace(r.FormValue("winner"))
		if raw := strings.TrimSpace(r.FormValue("losers")); raw != "" {
			for _, part := range strings.Split(raw, ",") {
				if v := strings.TrimSpace(part); v != "" {
					loserIDs = append(loserIDs, v)
				}
			}
		} else if loser := strings.TrimSpace(r.FormValue("loser")); loser != "" {
			loserIDs = append(loserIDs, loser)
		}
	}

	if winnerID == "" || len(loserIDs) == 0 {
		http.Error(w, "Invalid IDs", http.StatusBadRequest)
		return
	}

	seen := map[string]bool{winnerID: true}
	uniqLosers := make([]string, 0, len(loserIDs))
	for _, loserID := range loserIDs {
		if loserID == "" || seen[loserID] || loserID == winnerID {
			continue
		}
		seen[loserID] = true
		uniqLosers = append(uniqLosers, loserID)
	}
	if len(uniqLosers) == 0 {
		http.Error(w, "Invalid IDs", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	tx, err := a.DB.Pool().Begin(ctx)
	if err != nil {
		http.Error(w, "Failed to begin transaction", http.StatusInternalServerError)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var winnerTags []string
	if err := tx.QueryRow(ctx, `SELECT tags FROM images WHERE id = $1`, winnerID).Scan(&winnerTags); err != nil {
		http.Error(w, "Winner not found", http.StatusNotFound)
		return
	}

	mergedInput := append([]string{}, winnerTags...)
	for _, loserID := range uniqLosers {
		var loserTags []string
		if err := tx.QueryRow(ctx, `SELECT tags FROM images WHERE id = $1`, loserID).Scan(&loserTags); err != nil {
			http.Error(w, "Loser not found", http.StatusNotFound)
			return
		}
		mergedInput = append(mergedInput, loserTags...)
	}

	mergedTags := a.DB.ResolveTags(ctx, mergedInput)
	if mergedTags == nil {
		mergedTags = []string{}
	}

	if _, err := tx.Exec(ctx, `UPDATE images SET tags = $2 WHERE id = $1`, winnerID, mergedTags); err != nil {
		http.Error(w, "Failed to update winner tags", http.StatusInternalServerError)
		return
	}

	for _, loserID := range uniqLosers {
		if _, err := tx.Exec(ctx,
			`UPDATE seed_resolutions SET image_id = $1
			 WHERE image_id = $2
			 AND NOT EXISTS (
			   SELECT 1 FROM seed_resolutions sr2
			   WHERE sr2.seed = seed_resolutions.seed
			   AND sr2.tag = seed_resolutions.tag
			   AND sr2.image_id = $1
			 )`,
			winnerID, loserID); err != nil {
			http.Error(w, "Failed to rewrite seeds", http.StatusInternalServerError)
			return
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM images WHERE id = ANY($1)`, uniqLosers); err != nil {
		http.Error(w, "Failed to delete losers", http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "Failed to commit merge", http.StatusInternalServerError)
		return
	}

	if a.SFTP != nil {
		for _, loserID := range uniqLosers {
			_ = a.SFTP.Delete(loserID)
		}
	} else {
		for _, loserID := range uniqLosers {
			for _, ext := range imageformat.SupportedExtensions {
				os.Remove(filepath.Join(a.StoragePath, loserID+ext))
			}
		}
	}

	w.WriteHeader(http.StatusNoContent)
}


func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// ── Tag Registry handlers ─────────────────────────────────────────────────────

func (a *Admin) handleTags(w http.ResponseWriter, r *http.Request) {
	tags, err := a.DB.ListTagRegistry(r.Context())
	if err != nil {
		http.Error(w, "Database error", 500)
		return
	}
	// Count photos per tag
	photoCounts := map[string]int{}
	images, _ := a.DB.ListAllWithTags(r.Context())
	for _, img := range images {
		for _, t := range img.Tags {
			photoCounts[t]++
		}
	}

	// Count seed resolutions per tag
	seedCounts := map[string]int{}
	seedRows, err := a.DB.Pool().Query(r.Context(),
		`SELECT tag, COUNT(*) FROM seed_resolutions WHERE tag != '' GROUP BY tag`)
	if err == nil {
		defer seedRows.Close()
		for seedRows.Next() {
			var t string
			var c int
			if seedRows.Scan(&t, &c) == nil {
				seedCounts[t] = c
			}
		}
	}

	if tags == nil {
		tags = []postgres.TagEntry{}
	}
	a.render(w, "tags.html", map[string]any{
		"Page": "tags", "Tags": tags, "PhotoCounts": photoCounts, "SeedCounts": seedCounts,
		"Success": r.URL.Query().Get("success"),
		"Error":   r.URL.Query().Get("error"),
	})
}

func (a *Admin) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Redirect(w, r, "/admin/tags?error=Name+is+required", http.StatusFound)
		return
	}
	aliases := parseCommaList(r.FormValue("aliases"))
	if _, err := a.DB.CreateTag(r.Context(), name, aliases); err != nil {
		http.Redirect(w, r, "/admin/tags?error="+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/tags?success=Tag+created", http.StatusFound)
}

func (a *Admin) handleUpdateTagEntry(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	name := strings.TrimSpace(r.FormValue("name"))
	aliases := parseCommaList(r.FormValue("aliases"))
	if err := a.DB.UpdateTag(r.Context(), id, name, aliases); err != nil {
		http.Redirect(w, r, "/admin/tags?error="+url.QueryEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/tags?success=Tag+updated", http.StatusFound)
}

func (a *Admin) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if err := a.DB.DeleteTag(r.Context(), id); err != nil {
		http.Redirect(w, r, "/admin/tags?error=Failed+to+delete+tag", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/tags?success=Tag+deleted", http.StatusFound)
}

func parseCommaList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (a *Admin) handleNextID(w http.ResponseWriter, r *http.Request) {
	nextID, _ := a.DB.NextID(r.Context())
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"next_id": nextID})
}

// handleMeta returns registry tags and authors for picklist population.
func (a *Admin) handleMeta(w http.ResponseWriter, r *http.Request) {
	tagEntries, _ := a.DB.ListTagRegistry(r.Context())
	tags := make([]string, 0, len(tagEntries))
	for _, tag := range tagEntries {
		if tag.Name != "" {
			tags = append(tags, tag.Name)
		}
	}
	authors, _ := a.DB.ListDistinctAuthors(r.Context())
	if tags == nil {
		tags = []string{}
	}
	if authors == nil {
		authors = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"registryTags": tags, "authors": authors})
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

// render writes templates through a buffer so partial output is never sent if execution fails.
func (a *Admin) render(w http.ResponseWriter, tmpl string, data any) {
	var buf bytes.Buffer
	if err := a.templates.ExecuteTemplate(&buf, tmpl, data); err != nil {
		http.Error(w, "Template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}
