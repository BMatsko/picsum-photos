package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/DMarby/picsum-photos/internal/storage"
)

// Provider implements a file-based image storage
type Provider struct {
	path string
}

// New returns a new Provider instance
func New(path string) (*Provider, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}

	return &Provider{
		path,
	}, nil
}

// Get returns the image data for an image id, trying .jpg then .png.
func (p *Provider) Get(ctx context.Context, id string) ([]byte, error) {
	for _, ext := range []string{".jpg", ".png"} {
		data, err := os.ReadFile(filepath.Join(p.path, id+ext))
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, storage.ErrNotFound
}
