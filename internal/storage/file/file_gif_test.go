package file_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/DMarby/picsum-photos/internal/storage/file"
)

func TestFileProviderGetSupportsGIF(t *testing.T) {
	dir := t.TempDir()
	gifData := []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")
	if err := os.WriteFile(filepath.Join(dir, "410.gif"), gifData, 0644); err != nil {
		t.Fatal(err)
	}

	provider, err := file.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	buf, err := provider.Get(context.Background(), "410")
	if err != nil {
		t.Fatal(err)
	}
	if string(buf) != string(gifData) {
		t.Fatalf("unexpected gif bytes: got %d bytes, want %d", len(buf), len(gifData))
	}
}
