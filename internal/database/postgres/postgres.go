package postgres

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"

	"github.com/DMarby/picsum-photos/internal/database"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Provider implements a PostgreSQL-backed image database
type Provider struct {
	pool *pgxpool.Pool
}

const schema = `
CREATE TABLE IF NOT EXISTS images (
	id      TEXT PRIMARY KEY,
	author  TEXT NOT NULL DEFAULT '',
	url     TEXT NOT NULL DEFAULT '',
	width   INTEGER NOT NULL DEFAULT 0,
	height  INTEGER NOT NULL DEFAULT 0,
	tags    TEXT[] NOT NULL DEFAULT '{}'
);

-- Add tags column to existing tables that predate it
ALTER TABLE images ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';

CREATE TABLE IF NOT EXISTS seed_resolutions (
	seed       TEXT PRIMARY KEY,
	image_id   TEXT REFERENCES images(id) ON DELETE SET NULL,
	tag        TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Migrate existing tables: add tag column, change cascade to SET NULL so tag survives deletes
ALTER TABLE seed_resolutions ADD COLUMN IF NOT EXISTS tag TEXT NOT NULL DEFAULT '';
ALTER TABLE seed_resolutions ALTER COLUMN image_id DROP NOT NULL;
DO $$ BEGIN
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

// GetRandomWithSeed returns a deterministic image for the given seed.
// Pool order is sorted by id for stability.
func (p *Provider) GetRandomWithSeed(ctx context.Context, seed int64) (*database.Image, error) {
	return p.getRandomWithSeedAndTag(ctx, seed, "")
}

// GetRandomWithSeedAndTag resolves a seed, optionally filtering by tag.
//
// Resolution order:
//  1. If a stored resolution exists for this seed AND the image still exists, return it.
//  2. If a stored resolution row exists but the image was deleted (cascade removed the row),
//     OR no row exists: look up any stored tag for this seed (from a tag-only row),
//     then re-resolve using that stored tag (falling back to the request tag, then any).
//  3. Store the new resolution and tag.
func (p *Provider) GetRandomWithSeedAndTag(ctx context.Context, seed int64, seedStr string, tag string) (*database.Image, error) {
	// 1. Look up any existing row for this seed (image_id may be NULL if its image was deleted)
	var storedImageID *string
	var storedTag string
	p.pool.QueryRow(ctx,
		`SELECT image_id, tag FROM seed_resolutions WHERE seed = $1`, seedStr,
	).Scan(&storedImageID, &storedTag)

	// If image_id is non-null, the image still exists — return it directly
	if storedImageID != nil {
		row := p.pool.QueryRow(ctx,
			`SELECT id, author, url, width, height FROM images WHERE id = $1`, *storedImageID)
		img := &database.Image{}
		if err := row.Scan(&img.ID, &img.Author, &img.URL, &img.Width, &img.Height); err == nil {
			return img, nil
		}
	}

	// 2. No valid resolution (never set, or image was deleted and image_id is now NULL).
	//    Stored tag takes precedence over the incoming request tag; blank = no filter.
	resolveTag := storedTag
	if resolveTag == "" {
		resolveTag = tag
	}

	// 3. Pick from pool using resolveTag; get back the tag actually used (may differ if tag had no matches)
	resolved, effectiveTag, err := p.pickWithTag(ctx, seed, resolveTag)
	if err != nil {
		return nil, err
	}

	// 4. Store the resolution AND the effective tag (upsert)
	_, _ = p.pool.Exec(ctx,
		`INSERT INTO seed_resolutions (seed, image_id, tag) VALUES ($1, $2, $3)
		 ON CONFLICT (seed) DO UPDATE SET image_id = EXCLUDED.image_id, tag = EXCLUDED.tag`,
		seedStr, resolved.ID, effectiveTag)

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
	return p.list(ctx, 0, 1<<31)
}

// List returns a paginated slice of images sorted by ID.
func (p *Provider) List(ctx context.Context, offset, limit int) ([]database.Image, error) {
	return p.list(ctx, offset, limit)
}

func (p *Provider) list(ctx context.Context, offset, limit int) ([]database.Image, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, author, url, width, height FROM images ORDER BY id LIMIT $1 OFFSET $2`,
		limit, offset)
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

// ListAllWithTags returns images including their tags (for admin use).
func (p *Provider) ListAllWithTags(ctx context.Context) ([]ImageWithTags, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, author, url, width, height, tags FROM images ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var images []ImageWithTags
	for rows.Next() {
		img := ImageWithTags{}
		if err := rows.Scan(&img.ID, &img.Author, &img.URL, &img.Width, &img.Height, &img.Tags); err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, nil
}

// ImageWithTags extends Image with the tags array.
type ImageWithTags struct {
	database.Image
	Tags []string
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
