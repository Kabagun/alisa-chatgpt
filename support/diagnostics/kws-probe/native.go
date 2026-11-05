package main

/*
#cgo linux LDFLAGS: -ldl
#include <stdlib.h>
#include "station_native.h"
*/
import "C"

import (
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

const stockModel = "/system/vendor/quasar/spotter_models/activation/yandex"

type detector interface {
	feed([]int16) (bool, bool)
	close() bool
}

type nativeDetector struct {
	pointer *C.station_spotter
	overlay string
}

func modelFlags(model string) ([]byte, bool) {
	b, err := os.ReadFile(filepath.Join(model, "flags.txt"))
	if err != nil || len(b) > 65536 {
		return nil, false
	}
	text := string(b)
	for _, name := range []string{"freq-filter-enabled", "use-tts-blocker"} {
		if !strings.Contains(text, name+" true") {
			return nil, false
		}
		text = strings.ReplaceAll(text, name+" true", name+" false")
	}
	return []byte(text), true
}

func makeOverlay(root, model string) (string, bool) {
	flags, ok := modelFlags(model)
	if !ok {
		return "", false
	}
	dir, err := os.MkdirTemp(root, "model-")
	if err != nil {
		return "", false
	}
	for _, name := range []string{"acoustic_model.nnet", "words.txt"} {
		if os.Symlink(filepath.Join(model, name), filepath.Join(dir, name)) != nil {
			_ = removeOverlay(root, dir, model)
			return "", false
		}
	}
	if os.WriteFile(filepath.Join(dir, "flags.txt"), flags, 0600) != nil {
		_ = removeOverlay(root, dir, model)
		return "", false
	}
	return dir, true
}

func removeOverlay(root, dir, model string) bool {
	if filepath.Dir(dir) != root || !strings.HasPrefix(filepath.Base(dir), "model-") || !privateDirectory(dir) {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)
		switch name {
		case "acoustic_model.nnet", "words.txt":
			link, err := os.Readlink(path)
			if err != nil || link != filepath.Join(model, name) {
				return false
			}
		case "flags.txt":
			if !privateFile(path, 65536) {
				return false
			}
		default:
			return false
		}
	}
	for _, entry := range entries {
		if os.Remove(filepath.Join(dir, entry.Name())) != nil {
			return false
		}
	}
	return os.Remove(dir) == nil
}

func openNative(root string) (detector, bool) {
	dir, ok := makeOverlay(root, stockModel)
	if !ok {
		return nil, false
	}
	model := C.CString(dir)
	defer C.free(unsafe.Pointer(model))
	var message [512]C.char
	p := C.station_spotter_open(model, &message[0], C.size_t(len(message)))
	if p == nil {
		_ = removeOverlay(root, dir, stockModel)
		return nil, false
	}
	return &nativeDetector{pointer: p, overlay: dir}, true
}

func (d *nativeDetector) feed(pcm []int16) (bool, bool) {
	if len(pcm) != 320 || d.pointer == nil {
		return false, false
	}
	var message [512]C.char
	n := C.station_spotter_feed(d.pointer, (*C.int16_t)(unsafe.Pointer(&pcm[0])), 640, &message[0], C.size_t(len(message)))
	return n == 1, n >= 0
}

func (d *nativeDetector) close() bool {
	if d.pointer == nil {
		return true
	}
	var message [512]C.char
	n := C.station_spotter_close(d.pointer, &message[0], C.size_t(len(message)))
	d.pointer = nil
	return removeOverlay(filepath.Dir(d.overlay), d.overlay, stockModel) && n == 0
}
