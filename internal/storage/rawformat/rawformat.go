// Package rawformat resolves the stored file extension for an image ID
// to support the ".raw" output format (return image in its native format).
package rawformat

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	imageformat "github.com/DMarby/picsum-photos/internal/storage/format"
)

// Resolver finds the actual stored extension for an image ID.
type Resolver interface {
	StoredExtension(ctx context.Context, id string) string
}

// FileResolver checks the filesystem for a stored image file.
type FileResolver struct {
	BasePath string
}

func (r *FileResolver) StoredExtension(ctx context.Context, id string) string {
	lookupID := normalizeStorageID(id)
	for _, ext := range imageformat.SupportedExtensions {
		if _, err := os.Stat(filepath.Join(r.BasePath, lookupID+ext)); err == nil {
			return ext
		}
	}
	return ".jpg"
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
