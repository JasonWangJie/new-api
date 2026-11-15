package service

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrImageStorageBusy = errors.New("image storage upload queue is full")

type imageUploadLeaseKey struct{}

type ImageStorageUploadStatus struct {
	Limit  int `json:"limit"`
	Active int `json:"active"`
	Queued int `json:"queued"`
}

// A capacity lease covers acquiring bytes, validation and the complete storage
// round trip. Nested archive/storage calls share their caller's lease.
type imageUploadCapacity struct {
	mu       sync.Mutex
	changed  chan struct{}
	limit    int
	active   int
	queued   int
	revision int64
}

var imageStorageUploads = imageUploadCapacity{limit: 4, changed: make(chan struct{})}

func (capacity *imageUploadCapacity) configure(limit int, revision int64) {
	capacity.mu.Lock()
	defer capacity.mu.Unlock()
	if revision < capacity.revision || limit < 1 || limit > 32 {
		return
	}
	capacity.revision = revision
	if capacity.limit != limit {
		capacity.limit = limit
		close(capacity.changed)
		capacity.changed = make(chan struct{})
	}
}

func (capacity *imageUploadCapacity) acquire(ctx context.Context, limit int, revision int64) (context.Context, func(), error) {
	if ctx.Value(imageUploadLeaseKey{}) == capacity {
		return ctx, func() {}, nil
	}
	if limit < 1 || limit > 32 {
		return ctx, nil, errors.New("invalid image storage upload concurrency")
	}
	capacity.mu.Lock()
	if revision >= capacity.revision && capacity.limit != limit {
		capacity.limit = limit
		close(capacity.changed)
		capacity.changed = make(chan struct{})
	}
	capacity.revision = max(capacity.revision, revision)
	if capacity.queued >= 256 {
		capacity.mu.Unlock()
		return ctx, nil, ErrImageStorageBusy
	}
	capacity.queued++
	for {
		if err := ctx.Err(); err != nil {
			capacity.queued--
			capacity.mu.Unlock()
			return ctx, nil, err
		}
		if capacity.active < capacity.limit {
			capacity.queued--
			capacity.active++
			capacity.mu.Unlock()
			var released sync.Once
			return context.WithValue(ctx, imageUploadLeaseKey{}, capacity), func() {
				released.Do(func() {
					capacity.mu.Lock()
					capacity.active--
					close(capacity.changed)
					capacity.changed = make(chan struct{})
					capacity.mu.Unlock()
				})
			}, nil
		}
		changed := capacity.changed
		capacity.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		capacity.mu.Lock()
	}
}

func AcquireImageStorageUpload(ctx context.Context, cfg ImageRuntimeConfig) (context.Context, func(), error) {
	if ImageStorageUploadReserved(ctx) {
		return ctx, func() {}, nil
	}
	// Waiting for capacity is part of the upload deadline too. Otherwise a
	// bounded queue of slow uploads could retain HTTP requests for hours.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.UploadTimeout)*time.Second)
	ctx, release, err := imageStorageUploads.acquire(ctx, cfg.StorageUploadConcurrency, cfg.Revision)
	if err != nil {
		cancel()
		return ctx, nil, err
	}
	return ctx, func() {
		release()
		cancel()
	}, nil
}

func ImageStorageUploadReserved(ctx context.Context) bool {
	return ctx.Value(imageUploadLeaseKey{}) == &imageStorageUploads
}

func GetImageStorageUploadStatus() ImageStorageUploadStatus {
	imageStorageUploads.mu.Lock()
	defer imageStorageUploads.mu.Unlock()
	return ImageStorageUploadStatus{Limit: imageStorageUploads.limit, Active: imageStorageUploads.active, Queued: imageStorageUploads.queued}
}
