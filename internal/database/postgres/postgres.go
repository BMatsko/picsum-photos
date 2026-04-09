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
	height  INTEGER NOT NULL DEFAULT 0
);
`

// New connects to Postgres, runs migrations, and returns a Provider.
// databaseURL should be a standard postgres:// connection string.
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
// We load all IDs (sorted) and use Go's seeded rand to pick one —
// same behaviour as the file provider, compatible with existing seeds.
func (p *Provider) GetRandomWithSeed(ctx context.Context, seed int64) (*database.Image, error) {
	rows, err := p.pool.Query(ctx,
		`SELECT id, author, url, width, height FROM images ORDER BY id`)
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

// Pool exposes the underlying pgxpool for direct queries (e.g. admin UI).
func (p *Provider) Pool() *pgxpool.Pool {
	return p.pool
}
