package postgres

import (
	"context"
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
	image_id   TEXT NOT NULL REFERENCES images(id) ON DELETE CASCADE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_seed_resolutions_seed ON seed_resolutions(seed);
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
//  1. If a stored resolution exists for this seed, return that image (ignoring tag).
//  2. Otherwise pick deterministically from the tag-filtered pool (or full pool if tag=""),
//     store the resolution, and return the image.
func (p *Provider) GetRandomWithSeedAndTag(ctx context.Context, seed int64, seedStr string, tag string) (*database.Image, error) {
	// 1. Check for an existing stored resolution
	row := p.pool.QueryRow(ctx,
		`SELECT i.id, i.author, i.url, i.width, i.height
		 FROM seed_resolutions sr
		 JOIN images i ON i.id = sr.image_id
		 WHERE sr.seed = $1`, seedStr)
	img := &database.Image{}
	if err := row.Scan(&img.ID, &img.Author, &img.URL, &img.Width, &img.Height); err == nil {
		return img, nil
	}

	// 2. No stored resolution — pick from pool (filtered by tag if provided)
	resolved, err := p.getRandomWithSeedAndTag(ctx, seed, tag)
	if err != nil {
		return nil, err
	}

	// 3. Store the resolution (ignore conflict — race condition is fine)
	_, _ = p.pool.Exec(ctx,
		`INSERT INTO seed_resolutions (seed, image_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		seedStr, resolved.ID)

	return resolved, nil
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
