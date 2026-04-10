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
func (a *Admin) handleMerge(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/admin/dedupe?error=Bad+request", http.StatusFound)
		return
	}
	winnerID := strings.TrimSpace(r.FormValue("winner"))
	loserID := strings.TrimSpace(r.FormValue("loser"))
	if winnerID == "" || loserID == "" || winnerID == loserID {
		http.Redirect(w, r, "/admin/dedupe?error=Invalid+IDs", http.StatusFound)
		return
	}

	ctx := r.Context()

	// 1. Load both images
	winner, err := a.DB.Pool().Query(ctx, `SELECT tags FROM images WHERE id = $1`, winnerID)
	if err != nil {
		http.Redirect(w, r, "/admin/dedupe?error=Winner+not+found", http.StatusFound)
		return
	}
	var winnerTags []string
	if winner.Next() { winner.Scan(&winnerTags) }
	winner.Close()

	var loserTags []string
	loserRow, err := a.DB.Pool().Query(ctx, `SELECT tags FROM images WHERE id = $1`, loserID)
	if err == nil {
		if loserRow.Next() { loserRow.Scan(&loserTags) }
		loserRow.Close()
	}

	// 2. Union tags (lowercase, deduplicated)
	tagSet := map[string]struct{}{}
	for _, t := range winnerTags { tagSet[t] = struct{}{} }
	for _, t := range loserTags { tagSet[t] = struct{}{} }
	mergedTags := make([]string, 0, len(tagSet))
	for t := range tagSet { mergedTags = append(mergedTags, t) }
	sort.Strings(mergedTags)

	// 3. Write merged tags to winner
	_, err = a.DB.Pool().Exec(ctx, `UPDATE images SET tags = $2 WHERE id = $1`, winnerID, mergedTags)
	if err != nil {
		http.Redirect(w, r, "/admin/dedupe?error=Failed+to+update+winner+tags", http.StatusFound)
		return
	}

	// 4. Rewrite seed_resolutions: loser → winner
	// For any (seed, tag) that the loser owns but winner doesn't already have,
	// update image_id to winner. Where there's a conflict (winner already has that slot),
	// the winner's resolution takes precedence — just delete the loser's.
	_, err = a.DB.Pool().Exec(ctx,
		`UPDATE seed_resolutions SET image_id = $1
		 WHERE image_id = $2
		 AND NOT EXISTS (
		   SELECT 1 FROM seed_resolutions sr2
		   WHERE sr2.seed = seed_resolutions.seed
		   AND sr2.tag = seed_resolutions.tag
		   AND sr2.image_id = $1
		 )`,
		winnerID, loserID)
	if err != nil {
		http.Redirect(w, r, "/admin/dedupe?error=Failed+to+rewrite+seeds", http.StatusFound)
		return
	}

	// 5. Delete loser from DB (remaining seed_resolutions with loser will SET NULL via FK)
	if _, err = a.DB.Pool().Exec(ctx, `DELETE FROM images WHERE id = $1`, loserID); err != nil {
		http.Redirect(w, r, "/admin/dedupe?error=Failed+to+delete+loser", http.StatusFound)
		return
	}

	// 6. Delete loser file from storage
	if a.SFTP != nil {
		_ = a.SFTP.Delete(loserID)
	} else {
		for _, ext := range []string{".jpg", ".jpeg", ".png", ".webp", ".gif", ".heic", ".heif", ".avif", ".tiff", ".tif"} {
			os.Remove(filepath.Join(a.StoragePath, loserID+ext))
		}
	}

	http.Redirect(w, r,
		fmt.Sprintf("/admin/dedupe?success=Merged+%%23%s+into+%%23%s", loserID, winnerID),
		http.StatusFound)
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

// handleMeta returns distinct tags and authors for picklist population.
func (a *Admin) handleMeta(w http.ResponseWriter, r *http.Request) {
	tags, _ := a.DB.ListDistinctTags(r.Context())
	authors, _ := a.DB.ListDistinctAuthors(r.Context())
	if tags == nil {
		tags = []string{}
	}
	if authors == nil {
		authors = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"tags": tags, "authors": authors})
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
