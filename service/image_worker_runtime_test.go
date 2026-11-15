package service

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func imageRuntimePoolFixture(t *testing.T, ctx context.Context) *asyncImageWorkerPool {
	t.Helper()
	pool := &asyncImageWorkerPool{ctx: ctx, workers: make(map[*asyncImageWorker]struct{}), recoveryUpdates: make(chan int, 1)}
	asyncImageWorkerPools.Lock()
	previous := asyncImageWorkerPools.pools
	asyncImageWorkerPools.pools = append(asyncImageWorkerPools.pools, pool)
	asyncImageWorkerPools.Unlock()
	t.Cleanup(func() {
		asyncImageWorkerPools.Lock()
		asyncImageWorkerPools.pools = previous
		asyncImageWorkerPools.Unlock()
	})
	return pool
}

func TestAsyncImageWorkerRuntimeResizeDrainsActiveTasks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		pool := imageRuntimePoolFixture(t, ctx)
		finishTasks := make(chan struct{})
		completed := make(chan error, 4)
		run := func(ctx context.Context, retire <-chan struct{}) {
			// The task does not observe retirement until its current work ends.
			<-finishTasks
			completed <- ctx.Err()
			select {
			case <-retire:
			case <-ctx.Done():
			}
		}
		pool.resizeWorkers(3, run)
		synctest.Wait()
		require.Equal(t, 3, GetAsyncImageWorkerStatus().ActiveWorkers)
		pool.resizeWorkers(1, run)
		synctest.Wait()
		status := GetAsyncImageWorkerStatus()
		assert.Equal(t, 1, status.WorkerConcurrency)
		assert.Equal(t, 3, status.ActiveWorkers, "busy retiring workers remain visible")
		assert.NoError(t, ctx.Err(), "shrinking must not cancel ongoing task work")
		pool.resizeWorkers(2, run)
		synctest.Wait()
		assert.Equal(t, 4, GetAsyncImageWorkerStatus().ActiveWorkers, "new capacity is available while old workers drain")
		close(finishTasks)
		synctest.Wait()
		for range 4 {
			assert.NoError(t, <-completed, "all admitted tasks finish with their original context")
		}
		assert.Equal(t, 2, GetAsyncImageWorkerStatus().ActiveWorkers)
		cancel()
		pool.wg.Wait()
		assert.False(t, GetAsyncImageWorkerStatus().Running)
	})
}

func TestAsyncImageWorkerRuntimeRefreshRetainsLastGoodSettings(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		pool := imageRuntimePoolFixture(t, ctx)
		exitWorker := make(chan struct{})
		run := func(ctx context.Context, retire <-chan struct{}) {
			select {
			case <-ctx.Done():
			case <-retire:
			case <-exitWorker:
			}
		}
		pool.resizeWorkers(1, run)
		exitWorker <- struct{}{}
		synctest.Wait()
		assert.Zero(t, GetAsyncImageWorkerStatus().WorkerConcurrency, "a stopped worker is not reported as effective capacity")
		cfg := ImageRuntimeConfig{Workers: 3, RecoveryInterval: 15}
		refresh := make(chan struct{})
		proceed := make(chan struct{})
		var loadError error
		pool.wg.Go(func() {
			pool.supervise(func(context.Context) (ImageRuntimeConfig, error) {
				refresh <- struct{}{}
				<-proceed
				return cfg, loadError
			}, run)
		})
		<-refresh // synctest advances the refresh timer without wall-clock sleep.
		proceed <- struct{}{}
		synctest.Wait()
		assert.Equal(t, 3, GetAsyncImageWorkerStatus().WorkerConcurrency)
		assert.Equal(t, 3, GetAsyncImageWorkerStatus().ActiveWorkers)
		assert.Equal(t, 15, <-pool.recoveryUpdates)
		<-refresh
		loadError = errors.New("configuration unavailable")
		cfg.Workers = 8
		proceed <- struct{}{}
		synctest.Wait()
		assert.Equal(t, 3, GetAsyncImageWorkerStatus().WorkerConcurrency)
		assert.Empty(t, pool.recoveryUpdates, "a failed refresh does not publish partial configuration")
		<-refresh
		loadError = nil
		cfg.Workers, cfg.RecoveryInterval = 2, 7
		proceed <- struct{}{}
		synctest.Wait()
		assert.Equal(t, 2, GetAsyncImageWorkerStatus().ActiveWorkers)
		assert.Equal(t, 7, <-pool.recoveryUpdates)
		cancel()
		pool.wg.Wait()
	})
}

func TestAsyncImageWorkerRuntimeRecoveryIntervalAppliesWhileWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		pool := imageRuntimePoolFixture(t, ctx)
		recovered := make(chan time.Time, 1)
		pool.wg.Go(func() {
			pool.recoveryLoop(30, func() { recovered <- time.Now() })
		})
		started := <-recovered
		synctest.Wait()
		assert.Equal(t, 30, GetAsyncImageWorkerStatus().RecoveryIntervalSeconds)
		pool.recoveryUpdates <- 2
		synctest.Wait()
		assert.Equal(t, 2, GetAsyncImageWorkerStatus().RecoveryIntervalSeconds)
		assert.Equal(t, 2*time.Second, (<-recovered).Sub(started), "shorter interval replaces an existing long wait")
		synctest.Wait()
		pool.recoveryUpdates <- 7
		synctest.Wait()
		assert.Equal(t, 7, GetAsyncImageWorkerStatus().RecoveryIntervalSeconds)
		assert.Equal(t, 9*time.Second, (<-recovered).Sub(started))
		cancel()
		pool.wg.Wait()
	})
}

func TestAsyncImageWorkerRuntimeStatusTracksLivePool(t *testing.T) {
	first, stopFirst := context.WithCancel(t.Context())
	defer stopFirst()
	firstPool := imageRuntimePoolFixture(t, first)
	firstPool.recoverySeconds = 30
	for range 2 {
		firstPool.workers[&asyncImageWorker{}] = struct{}{}
	}
	second, stopSecond := context.WithCancel(t.Context())
	defer stopSecond()
	secondPool := imageRuntimePoolFixture(t, second)
	secondPool.recoverySeconds = 15
	for range 4 {
		secondPool.workers[&asyncImageWorker{}] = struct{}{}
	}
	assert.Equal(t, 4, GetAsyncImageWorkerStatus().WorkerConcurrency)
	stopFirst()
	assert.Equal(t, AsyncImageWorkerStatus{Running: true, WorkerConcurrency: 4, ActiveWorkers: 4, RecoveryIntervalSeconds: 15}, GetAsyncImageWorkerStatus())
	stopSecond()
	assert.Equal(t, AsyncImageWorkerStatus{}, GetAsyncImageWorkerStatus())
}

func TestImageStorageUploadCapacityDrainsAndCancels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		capacity := imageUploadCapacity{limit: 2, changed: make(chan struct{})}
		first, releaseFirst, err := capacity.acquire(t.Context(), 2, 1)
		require.NoError(t, err)
		_, releaseSecond, err := capacity.acquire(t.Context(), 2, 1)
		require.NoError(t, err)
		_, nestedRelease, err := capacity.acquire(first, 2, 1)
		require.NoError(t, err)
		nestedRelease()
		assert.Equal(t, 2, capacity.active, "nested writes must share one lease")
		queuedCtx, cancel := context.WithCancel(t.Context())
		completed := make(chan error, 1)
		go func() {
			_, release, err := capacity.acquire(queuedCtx, 1, 2)
			if release != nil {
				release()
			}
			completed <- err
		}()
		synctest.Wait()
		assert.Equal(t, 1, capacity.queued)
		releaseFirst()
		releaseFirst()
		synctest.Wait()
		assert.Equal(t, 1, capacity.active, "shrinking drains in-flight work without cancelling it")
		assert.Equal(t, 1, capacity.queued)
		cancel()
		synctest.Wait()
		require.ErrorIs(t, <-completed, context.Canceled)
		assert.Zero(t, capacity.queued)
		releaseSecond()
		assert.Zero(t, capacity.active)
		_, release, err := capacity.acquire(t.Context(), 2, 1)
		require.NoError(t, err)
		assert.Equal(t, 1, capacity.limit, "an older task snapshot must not undo a lower limit")
		release()
	})
}

func TestImageStorageUploadQueueBoundAndConfigCompatibility(t *testing.T) {
	capacity := imageUploadCapacity{limit: 1, changed: make(chan struct{}), queued: 256}
	_, release, err := capacity.acquire(t.Context(), 1, 0)
	require.ErrorIs(t, err, ErrImageStorageBusy)
	assert.Nil(t, release)
	assert.Zero(t, capacity.active)
	cfg := DefaultImageRuntimeConfig()
	require.NoError(t, common.UnmarshalJsonStr(`{"worker_concurrency":150,"image_concurrency":155,"result_retention_days":180,"task_retention_days":180}`, &cfg))
	require.NoError(t, cfg.Validate())
	assert.Equal(t, 4, cfg.StorageUploadConcurrency)
	assert.Equal(t, 180, cfg.ResultRetentionDays)
	assert.Equal(t, 150, cfg.Workers)
	for _, limit := range []int{0, 33} {
		cfg.StorageUploadConcurrency = limit
		assert.Error(t, cfg.Validate())
	}
	synctest.Test(t, func(t *testing.T) {
		cfg := DefaultImageRuntimeConfig()
		cfg.UploadTimeout = 5
		ctx, release, err := AcquireImageStorageUpload(t.Context(), cfg)
		require.NoError(t, err)
		defer release()
		deadline, present := ctx.Deadline()
		require.True(t, present, "capacity admission must establish a total upload deadline")
		assert.Equal(t, time.Now().Add(5*time.Second), deadline)
		nested, releaseNested, err := AcquireImageStorageUpload(ctx, cfg)
		require.NoError(t, err)
		defer releaseNested()
		assert.Same(t, ctx, nested, "nested uploads inherit the original deadline and capacity")
		time.Sleep(5 * time.Second)
		synctest.Wait()
		assert.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
	})
}
