package format_test

import (
	"testing"

	imageformat "github.com/DMarby/picsum-photos/internal/storage/format"
)

func TestDetectExtensionRecognizesGIF(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "gif87a", data: []byte("GIF87a\x01\x00\x01\x00")},
		{name: "gif89a", data: []byte("GIF89a\x01\x00\x01\x00")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imageformat.DetectExtension(tt.data); got != ".gif" {
				t.Fatalf("DetectExtension() = %q, want %q", got, ".gif")
			}
		})
	}
}
