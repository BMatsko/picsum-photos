// Package rawformat resolves the stored file extension for an image ID
// to support the ".raw" output format (return image in its native format).
package rawformat

import (
	"context"
	"os"
	"path/filepath"

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
	for _, ext := range imageformat.SupportedExtensions {
		if _, err := os.Stat(filepath.Join(r.BasePath, id+ext)); err == nil {
			return ext
		}
	}
	return ".jpg"
}
