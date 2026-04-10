package postgres

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"strings"

	"github.com/DMarby/picsum-photos/internal/database"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Provider implements a PostgreSQL-backed image database
type Provider struct {
	pool *pgxpool.Pool
}

const schema = `
CREATE TABLE IF NOT EXISTS images (
	id       TEXT PRIMARY KEY,
	author   TEXT NOT NULL DEFAULT '',
	url      TEXT NOT NULL DEFAULT '',
	filename TEXT NOT NULL DEFAULT '',
	width    INTEGER NOT NULL DEFAULT 0,
	height   INTEGER NOT NULL DEFAULT 0,
	tags     TEXT[] NOT NULL DEFAULT '{}'
);

-- Migrations for existing tables
ALTER TABLE images ADD COLUMN IF NOT EXISTS tags     TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE images ADD COLUMN IF NOT EXISTS filename TEXT   NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS seed_resolutions (
	seed       TEXT NOT NULL,
	tag        TEXT NOT NULL DEFAULT '',
	image_id   TEXT REFERENCES images(id) ON DELETE SET NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (seed, tag)
);

-- Migrate: ensure tag column exists and image_id is nullable
ALTER TABLE seed_resolutions ADD COLUMN IF NOT EXISTS tag TEXT NOT NULL DEFAULT '';
ALTER TABLE seed_resolutions ALTER COLUMN image_id DROP NOT NULL;

-- Migrate: upgrade from (seed) PK to (seed, tag) composite PK
-- Drop old single-column PK if it exists, then add composite PK
DO $$ BEGIN
  -- Drop old primary key on seed alone
  IF EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'seed_resolutions_pkey'
    AND contype = 'p'
    AND conrelid = 'seed_resolutions'::regclass
    AND array_length(conkey, 1) = 1
  ) THEN
    ALTER TABLE seed_resolutions DROP CONSTRAINT seed_resolutions_pkey;
    ALTER TABLE seed_resolutions ADD PRIMARY KEY (seed, tag);
  END IF;
  -- Fix FK to SET NULL if still CASCADE
  IF EXISTS (
    SELECT 1 FROM information_schema.table_constraints
    WHERE constraint_name = 'seed_resolutions_image_id_fkey'
  ) THEN
    ALTER TABLE seed_resolutions DROP CONSTRAINT seed_resolutions_image_id_fkey;
    ALTER TABLE seed_resolutions ADD CONSTRAINT seed_resolutions_image_id_fkey
      FOREIGN KEY (image_id) REFERENCES images(id) ON DELETE SET NULL;
  END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_seed_resolutions_seed ON seed_resolutions(seed);

CREATE TABLE IF NOT EXISTS api_keys (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL DEFAULT '',
	key_hash   TEXT NOT NULL UNIQUE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS tag_registry (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	aliases    TEXT[] NOT NULL DEFAULT '{}',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Index for fast alias lookup (unnest → GIN)
CREATE INDEX IF NOT EXISTS idx_tag_registry_aliases ON tag_registry USING GIN(aliases);
`

// New connects to Postgres, runs migrations, and returns a Provider.
func New(ctx context.Context, databaseURL string) (*Provider, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if _, err := pool.Exec(ctx, schema); err != nil {
		pool.Close()
		return nil, err
	}
	return &Provider{pool: pool}, nil
}

// Close shuts down the connection pool.
func (p *Provider) Close() {
	p.pool.Close()
}

// Pool exposes the underlying pgxpool for direct queries (e.g. admin UI).
func (p *Provider) Pool() *pgxpool.Pool {
	return p.pool
}

// Get returns the image with the given ID.
func (p *Provider) Get(ctx context.Context, id string) (*database.Image, error) {
	row := p.pool.QueryRow(ctx,
		`SELECT id, author, url, width, height FROM images WHERE id = $1`, id)
	img := &database.Image{}
	if err := row.Scan(&img.ID, &img.Author, &img.URL, &img.Width, &img.Height); err != nil {
		return nil, database.ErrNotFound
	}
	return img, nil
}

// GetRandom returns a random image.
func (p *Provider) GetRandom(ctx context.Context) (*database.Image, error) {
	row := p.pool.QueryRow(ctx,
		`SELECT id, author, url, width, height FROM images ORDER BY random() LIMIT 1`)
	img := &database.Image{}
	if err := row.Scan(&img.ID, &img.Author, &img.URL, &img.Width, &img.Height); err != nil {
		return nil, database.ErrNotFound
	}
	return img, nil
}

// GetRandomByAuthor returns a random image filtered by author (case-insensitive exact match).
func (p *Provider) GetRandomByAuthor(ctx context.Context, author string) (*database.Image, error) {
	row := p.pool.QueryRow(ctx,
		`SELECT id, author, url, width, height FROM images WHERE lower(author) = lower($1) ORDER BY random() LIMIT 1`,
		author)
	img := &database.Image{}
	if err := row.Scan(&img.ID, &img.Author, &img.URL, &img.Width, &img.Height); err != nil {
		// Fall back to any random image if author has none
		return p.GetRandom(ctx)
	}
	return img, nil
}

// GetRandomWithSeed returns a deterministic image for the given seed.
// Pool order is sorted by id for stability.
func (p *Provider) GetRandomWithSeed(ctx context.Context, seed int64) (*database.Image, error) {
	return p.getRandomWithSeedAndTag(ctx, seed, "")
}

// GetRandomWithSeedAndTag resolves a (seed, tag) pair to a specific image.
//
// Each unique (seed, tag) combination is stored and resolved independently.
// A bare request with no tag uses tag="" as its own slot.
//
// Resolution order:
//  1. If a stored (seed, tag) resolution exists and the image is still alive, return it.
//  2. If image was deleted (image_id is NULL), re-resolve using the same tag filter and update.
//  3. If no row exists yet, resolve fresh using the tag filter and store it.
//  4. If the tag has no matching images, fall back to the full pool (store with effective tag "").
func (p *Provider) GetRandomWithSeedAndTag(ctx context.Context, seed int64, seedStr string, tag string) (*database.Image, error) {
	// 1. Look up existing resolution for this exact (seed, tag) pair
	var storedImageID *string
	p.pool.QueryRow(ctx,
		`SELECT image_id FROM seed_resolutions WHERE seed = $1 AND tag = $2`,
		seedStr, tag,
	).Scan(&storedImageID)

	if storedImageID != nil {
		// Image still exists — return it
		row := p.pool.QueryRow(ctx,
			`SELECT id, author, url, width, height FROM images WHERE id = $1`, *storedImageID)
		img := &database.Image{}
		if err := row.Scan(&img.ID, &img.Author, &img.URL, &img.Width, &img.Height); err == nil {
			return img, nil
		}
		// Image was deleted — fall through to re-resolve below
	}

	// 2. No valid resolution — pick a new image filtered by this tag
	resolved, effectiveTag, err := p.pickWithTag(ctx, seed, tag)
	if err != nil {
		return nil, err
	}

	// 3. Store/update the resolution for this (seed, tag) pair
	_, _ = p.pool.Exec(ctx,
		`INSERT INTO seed_resolutions (seed, tag, image_id) VALUES ($1, $2, $3)
		 ON CONFLICT (seed, tag) DO UPDATE SET image_id = EXCLUDED.image_id`,
		seedStr, effectiveTag, resolved.ID)

	// If the tag had no matches and we fell back to empty, also store for the original tag key
	if effectiveTag != tag {
		_, _ = p.pool.Exec(ctx,
			`INSERT INTO seed_resolutions (seed, tag, image_id) VALUES ($1, $2, $3)
			 ON CONFLICT (seed, tag) DO UPDATE SET image_id = EXCLUDED.image_id`,
			seedStr, tag, resolved.ID)
	}

	return resolved, nil
}

// pickWithTag picks a deterministic image, returning both the image and the tag that was
// actually used for filtering (empty string if the requested tag had no matches and fell back).
func (p *Provider) pickWithTag(ctx context.Context, seed int64, tag string) (*database.Image, string, error) {
	img, err := p.getRandomWithSeedAndTag(ctx, seed, tag)
	if err != nil {
		return nil, "", err
	}
	// If we requested a tag but the result came from the full pool (fallback),
	// store "" so future re-resolutions don't retry a dead tag.
	// We detect this by re-checking if the tag pool is non-empty.
	effectiveTag := tag
	if tag != "" {
		var count int
		p.pool.QueryRow(ctx, `SELECT COUNT(*) FROM images WHERE $1 = ANY(tags)`, tag).Scan(&count)
		if count == 0 {
			effectiveTag = ""
		}
	}
	return img, effectiveTag, nil
}

func (p *Provider) getRandomWithSeedAndTag(ctx context.Context, seed int64, tag string) (*database.Image, error) {
	var rows interface {
		Next() bool
		Scan(...any) error
		Close()
		Err() error
	}
	var err error

	if tag != "" {
		r, e := p.pool.Query(ctx,
			`SELECT id, author, url, width, height FROM images WHERE $1 = ANY(tags) ORDER BY id`,
			tag)
		rows, err = r, e
	} else {
		r, e := p.pool.Query(ctx,
			`SELECT id, author, url, width, height FROM images ORDER BY id`)
		rows, err = r, e
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var images []database.Image
	for rows.Next() {
		img := database.Image{}
		if err := rows.Scan(&img.ID, &img.Author, &img.URL, &img.Width, &img.Height); err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	if len(images) == 0 {
		// Fall back to full pool if tag matched nothing
		if tag != "" {
			return p.getRandomWithSeedAndTag(ctx, seed, "")
		}
		return nil, database.ErrNotFound
	}

	r := rand.New(rand.NewSource(seed)) //nolint:gosec
	return &images[r.Intn(len(images))], nil
}

// ListAll returns every image sorted by ID.
func (p *Provider) ListAll(ctx context.Context) ([]database.Image, error) {
	return p.listFiltered(ctx, "", 0, 1<<31)
}

// List returns a paginated slice of images sorted by ID.
func (p *Provider) List(ctx context.Context, offset, limit int) ([]database.Image, error) {
	return p.listFiltered(ctx, "", offset, limit)
}

// ListByAuthor returns a paginated slice filtered by author (case-insensitive contains).
func (p *Provider) ListByAuthor(ctx context.Context, author string, offset, limit int) ([]database.Image, error) {
	return p.listFiltered(ctx, author, offset, limit)
}

func (p *Provider) listFiltered(ctx context.Context, author string, offset, limit int) ([]database.Image, error) {
	var rows interface {
		Next() bool
		Scan(...any) error
		Close()
		Err() error
	}
	var err error
	if author != "" {
		var r interface {
			Next() bool
			Scan(...any) error
			Close()
			Err() error
		}
		r, err = p.pool.Query(ctx,
			`SELECT id, author, url, width, height FROM images WHERE lower(author) = lower($1) ORDER BY id LIMIT $2 OFFSET $3`,
			author, limit, offset)
		rows = r
	} else {
		var r interface {
			Next() bool
			Scan(...any) error
			Close()
			Err() error
		}
		r, err = p.pool.Query(ctx,
			`SELECT id, author, url, width, height FROM images ORDER BY id LIMIT $1 OFFSET $2`,
			limit, offset)
		rows = r
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var images []database.Image
	for rows.Next() {
		img := database.Image{}
		if err := rows.Scan(&img.ID, &img.Author, &img.URL, &img.Width, &img.Height); err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, nil
}

// ListDistinctTags returns all unique tag values across all images, sorted.
func (p *Provider) ListDistinctTags(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT DISTINCT unnest(tags) AS tag FROM images ORDER BY tag`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err == nil && t != "" {
			tags = append(tags, t)
		}
	}
	return tags, nil
}

// ListDistinctAuthors returns all unique author values across all images, sorted.
func (p *Provider) ListDistinctAuthors(ctx context.Context) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT DISTINCT author FROM images WHERE author != '' ORDER BY author`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var authors []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err == nil {
			authors = append(authors, a)
		}
	}
	return authors, nil
}

// ListAllWithTags returns images including their tags and filename (for admin use).
func (p *Provider) ListAllWithTags(ctx context.Context) ([]ImageWithTags, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, author, url, filename, width, height, tags FROM images ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var images []ImageWithTags
	for rows.Next() {
		img := ImageWithTags{}
		if err := rows.Scan(&img.ID, &img.Author, &img.URL, &img.Filename, &img.Width, &img.Height, &img.Tags); err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, nil
}

// ImageWithTags extends Image with tags and the original upload filename.
type ImageWithTags struct {
	database.Image
	Tags     []string
	Filename string
}

// NextID returns max(numeric id) + 1, or 1 if the table is empty.
func (p *Provider) NextID(ctx context.Context) (int, error) {
	var maxID int
	row := p.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(id::integer), 0) FROM images WHERE id ~ '^\d+$'`)
	if err := row.Scan(&maxID); err != nil {
		return 1, nil
	}
	return maxID + 1, nil
}

// APIKey represents a stored API key record.
type APIKey struct {
	ID        string
	Name      string
	CreatedAt string
}

// CreateAPIKey generates a new random key, stores its SHA-256 hash, and returns
// both the record and the plaintext key (only available at creation time).
func (p *Provider) CreateAPIKey(ctx context.Context, name string) (APIKey, string, error) {
	// Generate 32 random bytes → 64-char hex key
	b := make([]byte, 32)
	if _, err := cryptorand.Read(b); err != nil {
		return APIKey{}, "", fmt.Errorf("generate key: %w", err)
	}
	plaintext := "pk_" + hex.EncodeToString(b)

	// SHA-256 hash stored in DB
	hashBytes := sha256.Sum256([]byte(plaintext))
	keyHash := hex.EncodeToString(hashBytes[:])

	// Random short ID
	idBytes := make([]byte, 8)
	cryptorand.Read(idBytes)
	id := hex.EncodeToString(idBytes)

	_, err := p.pool.Exec(ctx,
		`INSERT INTO api_keys (id, name, key_hash) VALUES ($1, $2, $3)`,
		id, name, keyHash,
	)
	if err != nil {
		return APIKey{}, "", fmt.Errorf("insert api key: %w", err)
	}
	return APIKey{ID: id, Name: name}, plaintext, nil
}

// LookupAPIKey checks whether the given plaintext key is valid.
// Returns the key record if found, ErrNotFound otherwise.
func (p *Provider) LookupAPIKey(ctx context.Context, plaintext string) (APIKey, error) {
	hashBytes := sha256.Sum256([]byte(plaintext))
	keyHash := hex.EncodeToString(hashBytes[:])
	row := p.pool.QueryRow(ctx,
		`SELECT id, name FROM api_keys WHERE key_hash = $1`, keyHash)
	var k APIKey
	if err := row.Scan(&k.ID, &k.Name); err != nil {
		return APIKey{}, database.ErrNotFound
	}
	return k, nil
}

// ListAPIKeys returns all API keys (without hashes).
func (p *Provider) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, to_char(created_at, 'Mon DD, YYYY HH24:MI') FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.Name, &k.CreatedAt); err == nil {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

// DeleteAPIKey removes a key by ID.
func (p *Provider) DeleteAPIKey(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, id)
	return err
}

// ── Tag Registry ───────────────────────────────────────────────────────────────

// TagEntry represents a registered tag with its canonical name and aliases.
type TagEntry struct {
	ID        string
	Name      string
	Aliases   []string
	CreatedAt string
}

// ListTagRegistry returns all registered tags sorted by name.
func (p *Provider) ListTagRegistry(ctx context.Context) ([]TagEntry, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, name, aliases, to_char(created_at, 'Mon DD, YYYY') FROM tag_registry ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tags []TagEntry
	for rows.Next() {
		var t TagEntry
		var aliases []string
		if err := rows.Scan(&t.ID, &t.Name, &aliases, &t.CreatedAt); err == nil {
			if aliases == nil {
				aliases = []string{}
			}
			t.Aliases = aliases
			tags = append(tags, t)
		}
	}
	return tags, nil
}

// CreateTag creates a new tag registry entry.
func (p *Provider) CreateTag(ctx context.Context, name string, aliases []string) (TagEntry, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return TagEntry{}, fmt.Errorf("tag name is required")
	}
	if aliases == nil {
		aliases = []string{}
	}
	// Normalise aliases
	for i, a := range aliases {
		aliases[i] = strings.ToLower(strings.TrimSpace(a))
	}

	idBytes := make([]byte, 8)
	cryptorand.Read(idBytes)
	id := hex.EncodeToString(idBytes)

	_, err := p.pool.Exec(ctx,
		`INSERT INTO tag_registry (id, name, aliases) VALUES ($1, $2, $3)`,
		id, name, aliases)
	if err != nil {
		return TagEntry{}, fmt.Errorf("create tag: %w", err)
	}
	return TagEntry{ID: id, Name: name, Aliases: aliases}, nil
}

// UpdateTag updates the name and aliases of an existing tag.
func (p *Provider) UpdateTag(ctx context.Context, id, name string, aliases []string) error {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return fmt.Errorf("tag name is required")
	}
	if aliases == nil {
		aliases = []string{}
	}
	for i, a := range aliases {
		aliases[i] = strings.ToLower(strings.TrimSpace(a))
	}
	_, err := p.pool.Exec(ctx,
		`UPDATE tag_registry SET name=$2, aliases=$3 WHERE id=$1`,
		id, name, aliases)
	return err
}

// DeleteTag removes a tag from the registry.
func (p *Provider) DeleteTag(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM tag_registry WHERE id=$1`, id)
	return err
}

// ResolveTag takes any tag string (canonical name or alias) and returns
// the canonical name. Returns the input unchanged if no match is found.
func (p *Provider) ResolveTag(ctx context.Context, tag string) string {
	if tag == "" {
		return ""
	}
	tag = strings.ToLower(strings.TrimSpace(tag))

	// Exact canonical name match
	var canonical string
	err := p.pool.QueryRow(ctx,
		`SELECT name FROM tag_registry WHERE name = $1`, tag).Scan(&canonical)
	if err == nil {
		return canonical
	}

	// Alias match
	err = p.pool.QueryRow(ctx,
		`SELECT name FROM tag_registry WHERE $1 = ANY(aliases)`, tag).Scan(&canonical)
	if err == nil {
		return canonical
	}

	// No registry entry — return as-is (unregistered tags still work)
	return tag
}
