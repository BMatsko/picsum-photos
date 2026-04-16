package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/DMarby/picsum-photos/internal/storage"
	imageformat "github.com/DMarby/picsum-photos/internal/storage/format"
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

// Get returns the image data for an image id, trying all supported extensions.
func (p *Provider) Get(ctx context.Context, id string) ([]byte, error) {
	lookupID := normalizeStorageID(id)
	for _, ext := range imageformat.SupportedExtensions {
		data, err := os.ReadFile(filepath.Join(p.path, lookupID+ext))
		if err == nil {
			return data, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return nil, storage.ErrNotFound
}

func normalizeStorageID(id string) string {
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
