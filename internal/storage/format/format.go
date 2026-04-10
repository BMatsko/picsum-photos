// Package format detects image file formats from magic bytes and maps
// MIME types / file extensions to canonical storage extensions.
package format

import "strings"

// All supported upload extensions, in order of lookup preference.
var SupportedExtensions = []string{".jpg", ".jpeg", ".png", ".webp", ".heic", ".heif", ".avif", ".tiff", ".tif"}

// DetectExtension inspects the first 16 bytes of file data and returns
// the canonical storage extension (e.g. ".jpg", ".png", ".heic").
// Falls back to ".jpg" if the format is unrecognised.
func DetectExtension(data []byte) string {
	if len(data) < 4 {
		return ".jpg"
	}

	// JPEG: FF D8 FF
	if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return ".jpg"
	}
	// PNG: 89 50 4E 47
	if data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return ".png"
	}
	// WebP: 52 49 46 46 .... 57 45 42 50
	if len(data) >= 12 &&
		data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46 &&
		data[8] == 0x57 && data[9] == 0x45 && data[10] == 0x42 && data[11] == 0x50 {
		return ".webp"
	}
	// TIFF: 49 49 2A 00 (little-endian) or 4D 4D 00 2A (big-endian)
	if (data[0] == 0x49 && data[1] == 0x49 && data[2] == 0x2A && data[3] == 0x00) ||
		(data[0] == 0x4D && data[1] == 0x4D && data[2] == 0x00 && data[3] == 0x2A) {
		return ".tiff"
	}
	// HEIC/HEIF/AVIF: ISO Base Media File Format — check ftyp box
	// Bytes 4-7 are "ftyp", bytes 8-11 are the brand
	if len(data) >= 12 && data[4] == 'f' && data[5] == 't' && data[6] == 'y' && data[7] == 'p' {
		brand := string(data[8:12])
		switch brand {
		case "heic", "heix", "heim", "heis", "hevc", "hevx":
			return ".heic"
		case "mif1", "msf1":
			return ".heif"
		case "avif", "avis":
			return ".avif"
		}
		// Generic ISOBMFF — assume HEIF
		return ".heif"
	}

	return ".jpg"
}

// ExtFromMIME maps a Content-Type to a canonical extension.
func ExtFromMIME(mime string) string {
	mime = strings.ToLower(strings.Split(mime, ";")[0])
	switch mime {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/heic":
		return ".heic"
	case "image/heif":
		return ".heif"
	case "image/avif":
		return ".avif"
	case "image/tiff":
		return ".tiff"
	default:
		return ""
	}
}

// IsSupported returns true if the extension is in the supported list.
func IsSupported(ext string) bool {
	ext = strings.ToLower(ext)
	for _, e := range SupportedExtensions {
		if e == ext {
			return true
		}
	}
	return false
}
