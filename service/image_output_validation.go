package service

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/QuantumNous/new-api/model"
	"github.com/samber/hot"
)

const imageOutputValidationCacheCapacity = 512

type imageOutputValidation struct {
	contentType string
	width       int
	height      int
	byteSize    int
}

type imageOutputValidationCache struct {
	once    sync.Once
	entries *hot.HotCache[string, imageOutputValidation]
}

var validatedImageOutputs imageOutputValidationCache

func (cache *imageOutputValidationCache) records() *hot.HotCache[string, imageOutputValidation] {
	cache.once.Do(func() {
		cache.entries = hot.NewHotCache[string, imageOutputValidation](hot.LRU, imageOutputValidationCacheCapacity).Build()
	})
	return cache.entries
}

// remember records metadata only after the output has passed full validation.
func (cache *imageOutputValidationCache) remember(image ImageBytes) {
	switch image.ContentType {
	case "image/png", "image/jpeg":
		cache.records().Set(image.Checksum, imageOutputValidation{
			contentType: image.ContentType,
			width:       image.Width,
			height:      image.Height,
			byteSize:    len(image.Data),
		})
	}
}

func (cache *imageOutputValidationCache) validateStaged(saved model.AsyncImageStagingObject, maxBytes, maxPixels int64) (ImageBytes, error) {
	record, found := cache.records().Peek(saved.Checksum)
	if found && record.contentType == saved.ContentType &&
		record.width == saved.Width && record.height == saved.Height &&
		record.byteSize == len(saved.Data) && int64(record.byteSize) <= maxBytes &&
		int64(record.width) <= maxPixels/int64(record.height) {
		sum := sha256.Sum256(saved.Data)
		if hex.EncodeToString(sum[:]) == saved.Checksum {
			return ImageBytes{
				Data:        saved.Data,
				ContentType: record.contentType,
				Checksum:    saved.Checksum,
				Width:       record.width,
				Height:      record.height,
			}, nil
		}
	}

	// A fresh process can recover staged outputs by running the same full check.
	image, err := validateStoredImageBytes(saved.Data, saved.ContentType, maxBytes, maxPixels)
	if err != nil {
		return ImageBytes{}, err
	}
	if image.Checksum != saved.Checksum {
		return ImageBytes{}, model.ErrImageConflict
	}
	cache.remember(image)
	return image, nil
}
