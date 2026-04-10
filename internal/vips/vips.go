package vips

/*
#cgo pkg-config: vips
#include <stdlib.h>
#include "vips-bridge.h"
*/
import "C"

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/DMarby/picsum-photos/internal/logger"
)

// Image is a representation of the *C.VipsImage type
type Image *C.VipsImage

var (
	once     sync.Once
	log      *logger.Logger
	errMutex sync.Mutex
)

// Initialize libvips if it's not already started
func Initialize(logger *logger.Logger) error {
	var err error

	once.Do(func() {
		// vips_init needs to run on the main thread
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if C.VIPS_MAJOR_VERSION != 8 || C.VIPS_MINOR_VERSION < 6 {
			err = fmt.Errorf("unsupported libvips version")
			return
		}

		cName := C.CString("picsum-photos")
		defer C.free(unsafe.Pointer(cName))

		errorCode := C.vips_init(cName)
		if errorCode != 0 {
			err = fmt.Errorf("unable to initialize vips: %v", catchVipsError())
			return
		}

		// Catch vips logging/warnings
		log = logger
		C.setup_logging()

		// Set concurrency to 1 so that each job only uses one thread
		C.vips_concurrency_set(1)

		// Disable the cache
		C.vips_cache_set_max_mem(0)
		C.vips_cache_set_max(0)

		// // Disable SIMD vector instructions due to g_object_unref segfault
		// C.vips_vector_set_enabled(C.int(0))
	})

	return err
}

// log_callback catches logs from libvips
//
//export log_callback
func log_callback(message *C.char) {
	log.Debug(C.GoString(message))
}

// Shutdown libvips
func Shutdown() {
	C.vips_shutdown()
}

// PrintDebugInfo prints libvips debug info to stdout
func PrintDebugInfo() {
	C.vips_object_print_all()
}

// catchVipsError returns the vips error buffer as an error
func catchVipsError() error {
	errMutex.Lock()
	defer errMutex.Unlock()
	defer C.vips_error_clear()

	s := C.GoString(C.vips_error_buffer())
	return fmt.Errorf("%s", s)
}

// ResizeImage loads an image from a buffer and resizes it.
func ResizeImage(buffer []byte, width int, height int) (Image, error) {
	if len(buffer) == 0 {
		return nil, fmt.Errorf("empty buffer")
	}

	imageBuffer := unsafe.Pointer(&buffer[0])
	imageBufferSize := C.size_t(len(buffer))

	var image *C.VipsImage

	errCode := C.resize_image(imageBuffer, imageBufferSize, &image, C.int(width), C.int(height), C.VIPS_INTERESTING_CENTRE)

	// Prevent buffer from being garbage collected until after resize_image has been called
	runtime.KeepAlive(buffer)

	if errCode != 0 {
		return nil, fmt.Errorf("error processing image from buffer %s", catchVipsError())
	}

	return image, nil
}

// SaveToJpegBuffer saves an image as JPEG to a buffer
func SaveToJpegBuffer(image Image) ([]byte, error) {
	defer UnrefImage(image)

	var bufferPointer unsafe.Pointer
	bufferLength := C.size_t(0)

	errCode := C.save_image_to_jpeg_buffer(image, &bufferPointer, &bufferLength)

	if errCode != 0 {
		return nil, fmt.Errorf("error saving to jpeg buffer %s", catchVipsError())
	}

	buffer := C.GoBytes(bufferPointer, C.int(bufferLength))

	C.g_free(C.gpointer(bufferPointer))

	return buffer, nil
}

// SaveToGifBuffer saves an image (all frames) as GIF to a buffer.
func SaveToGifBuffer(image Image) ([]byte, error) {
	defer UnrefImage(image)

	var bufferPointer unsafe.Pointer
	bufferLength := C.size_t(0)

	errCode := C.save_image_to_gif_buffer(image, &bufferPointer, &bufferLength)
	if errCode != 0 {
		return nil, fmt.Errorf("error saving to gif buffer %s", catchVipsError())
	}
	buffer := C.GoBytes(bufferPointer, C.int(bufferLength))
	C.g_free(C.gpointer(bufferPointer))
	return buffer, nil
}

// ResizeAnimated resizes an animated image (GIF/WebP) preserving all frames.
func ResizeAnimated(imageBuffer []byte, width, height int) (Image, error) {
	if len(imageBuffer) == 0 {
		return nil, fmt.Errorf("empty buffer")
	}

	var image *C.VipsImage

	errCode := C.resize_animated(
		unsafe.Pointer(&imageBuffer[0]),
		C.size_t(len(imageBuffer)),
		&image,
		C.int(width),
		C.int(height),
		C.VIPS_INTERESTING_CENTRE,
	)

	runtime.KeepAlive(imageBuffer)

	if errCode != 0 {
		return nil, fmt.Errorf("error resizing animated image %s", catchVipsError())
	}
	return image, nil
}

// SaveToTiffBuffer saves an image as TIFF to a buffer
func SaveToTiffBuffer(image Image) ([]byte, error) {
	defer UnrefImage(image)

	var bufferPointer unsafe.Pointer
	bufferLength := C.size_t(0)

	errCode := C.save_image_to_tiff_buffer(image, &bufferPointer, &bufferLength)
	if errCode != 0 {
		return nil, fmt.Errorf("error saving to tiff buffer %s", catchVipsError())
	}
	buffer := C.GoBytes(bufferPointer, C.int(bufferLength))
	C.g_free(C.gpointer(bufferPointer))
	return buffer, nil
}

// SaveToAvifBuffer saves an image as AVIF to a buffer
func SaveToAvifBuffer(image Image) ([]byte, error) {
	defer UnrefImage(image)

	var bufferPointer unsafe.Pointer
	bufferLength := C.size_t(0)

	errCode := C.save_image_to_avif_buffer(image, &bufferPointer, &bufferLength)
	if errCode != 0 {
		return nil, fmt.Errorf("error saving to avif buffer %s", catchVipsError())
	}
	buffer := C.GoBytes(bufferPointer, C.int(bufferLength))
	C.g_free(C.gpointer(bufferPointer))
	return buffer, nil
}

// SaveToPngBuffer saves an image as PNG to a buffer
func SaveToPngBuffer(image Image) ([]byte, error) {
	defer UnrefImage(image)

	var bufferPointer unsafe.Pointer
	bufferLength := C.size_t(0)

	errCode := C.save_image_to_png_buffer(image, &bufferPointer, &bufferLength)

	if errCode != 0 {
		return nil, fmt.Errorf("error saving to png buffer %s", catchVipsError())
	}

	buffer := C.GoBytes(bufferPointer, C.int(bufferLength))

	C.g_free(C.gpointer(bufferPointer))

	return buffer, nil
}

// SaveToWebPBuffer saves an image as WebP to a buffer
func SaveToWebPBuffer(image Image) ([]byte, error) {
	defer UnrefImage(image)

	var bufferPointer unsafe.Pointer
	bufferLength := C.size_t(0)

	errCode := C.save_image_to_webp_buffer(image, &bufferPointer, &bufferLength)

	if errCode != 0 {
		return nil, fmt.Errorf("error saving to webp buffer %s", catchVipsError())
	}

	buffer := C.GoBytes(bufferPointer, C.int(bufferLength))

	C.g_free(C.gpointer(bufferPointer))

	return buffer, nil
}

// Grayscale converts an image to grayscale
func Grayscale(image Image) (Image, error) {
	defer UnrefImage(image)

	var result *C.VipsImage

	errCode := C.change_colorspace(image, &result, C.VIPS_INTERPRETATION_B_W)

	if errCode != 0 {
		return nil, fmt.Errorf("error changing image colorspace %s", catchVipsError())
	}

	return result, nil
}

// Blur applies gaussian blur to an image
func Blur(image Image, blur int) (Image, error) {
	defer UnrefImage(image)

	var result *C.VipsImage

	errCode := C.blur_image(image, &result, C.double(blur))

	if errCode != 0 {
		return nil, fmt.Errorf("error applying blur to image %s", catchVipsError())
	}

	return result, nil
}

// SetUserComment sets the UserComment field in the exif metadata for an image
func SetUserComment(image Image, comment string) {
	cComment := C.CString(comment)
	defer C.free(unsafe.Pointer(cComment))
	C.set_user_comment(image, cComment)
}

// UnrefImage unrefs an image object
func UnrefImage(image Image) {
	if image != nil {
		C.g_object_unref(C.gpointer(image))
	}
}

// NewEmptyImage returns an empty image object
func NewEmptyImage() Image {
	return C.vips_image_new()
}
