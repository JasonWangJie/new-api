package service

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

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
