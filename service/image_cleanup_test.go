package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func imageCleanupFixture(t *testing.T) (*gorm.DB, model.ImageStorageProfile) {
	t.Helper()
	db, _, _, _, _ := asyncImageWorkerFixture(t)
	entities := []any{&model.ImageCleanupJob{}, &model.ImageCleanupRetry{}, &model.ImageCleanupSchedule{}, &model.ImageStorageObject{}, &model.ImageUploadIntent{}, &model.AsyncImageResult{}, &model.ImageLibraryItem{}, &model.ImagePublication{}, &model.ImageInputObject{}, &model.ImageInputTaskReference{}}
	require.NoError(t, db.AutoMigrate(entities...))
	require.NoError(t, db.AutoMigrate(entities...))
	require.NoError(t, model.MigrateImageStorageChannels(db))
	t.Cleanup(func() { assert.NoError(t, db.Migrator().DropTable(entities...)) })
	var profile model.ImageStorageProfile
	require.NoError(t, db.Where("profile_id = ?", "worker-local").Take(&profile).Error)
	imageCleanupLoad.mu.Lock()
	checked, reason := imageCleanupLoad.checked, imageCleanupLoad.reason
	imageCleanupLoad.checked, imageCleanupLoad.reason = time.Now().Add(time.Hour), ""
	imageCleanupLoad.mu.Unlock()
	t.Cleanup(func() {
		imageCleanupLoad.mu.Lock()
		imageCleanupLoad.checked, imageCleanupLoad.reason = checked, reason
		imageCleanupLoad.mu.Unlock()
	})
	return db, profile
}

func TestImageLibraryArchiveRunsIndependentlyFromCleanup(t *testing.T) {
	db, task, _, cfg, _ := asyncImageWorkerFixture(t)
	require.NoError(t, model.MigrateImageModels(db))
	cfg.AutoArchive = true
	_, err := SaveImageStorageProfile(t.Context(), model.ImageStorageProfile{Class: "durable", Backend: "local", Provider: "local", Root: t.TempDir(), Active: true}, nil)
	require.NoError(t, err)
	store, err := GetImageStorage(t.Context(), "temporary")
	require.NoError(t, err)
	image := asyncImageBytesFixture(t)
	now := time.Now().Unix()
	object, err := StoreImageIntent(t.Context(), model.ImageUploadIntent{IntentKey: "archive-source", TaskId: task.TaskId, UserId: task.UserId, ProfileId: store.Profile.ProfileId, ObjectKey: "archive-source.png", Class: "temporary", ContentType: image.ContentType, ByteSize: int64(len(image.Data)), Checksum: image.Checksum, Status: "pending", CreatedAt: now}, image, now+3600)
	require.NoError(t, err)
	require.NoError(t, db.Model(&task).Updates(map[string]any{"status": model.ImageTaskSucceeded, "billing_status": "succeeded", "image_count": 1, "result_count": 1, "expires_at": now + 3600}).Error)
	require.NoError(t, db.Create(&model.AsyncImageResult{TaskId: task.TaskId, ObjectId: object.ObjectId, CreatedAt: now}).Error)
	commands := []model.ImageOutbox{
		{EventKey: "archive-success", Kind: "library_archive", AggregateId: task.TaskId, Status: "pending", CreatedAt: now},
		{EventKey: "archive-disabled", Kind: "library_archive", AggregateId: task.TaskId, Status: "pending", CreatedAt: now},
		{EventKey: "archive-retry", Kind: "library_archive", AggregateId: "temporarily-unavailable-task", Status: "pending", CreatedAt: now},
	}
	require.NoError(t, db.Create(&commands).Error)
	require.NoError(t, MaintainImageStorage(t.Context(), cfg))
	var pending int64
	require.NoError(t, db.Model(&model.ImageOutbox{}).Where("kind = ? AND status = ?", "library_archive", "pending").Count(&pending).Error)
	assert.EqualValues(t, 3, pending, "cleanup must not run normal archive uploads in its short request budget")
	require.NoError(t, MaintainImageArchives(t.Context(), cfg))
	require.NoError(t, db.First(&commands[0], commands[0].Id).Error)
	assert.Equal(t, "delivered", commands[0].Status)
	var archived int64
	require.NoError(t, db.Model(&model.ImageLibraryItem{}).Where("user_id = ?", task.UserId).Count(&archived).Error)
	assert.EqualValues(t, 1, archived)
	require.NoError(t, db.Model(&model.ImageOutbox{}).Where("kind = ? AND status = ?", "library_archive", "pending").Count(&pending).Error)
	assert.EqualValues(t, 2, pending, "one dispatcher slice handles one archive command")
	cfg.AutoArchive = false
	require.NoError(t, MaintainImageArchives(t.Context(), cfg))
	require.NoError(t, db.First(&commands[1], commands[1].Id).Error)
	assert.Equal(t, "skipped", commands[1].Status)
	cfg.AutoArchive = true
	require.Error(t, MaintainImageArchives(t.Context(), cfg))
	require.NoError(t, db.First(&commands[2], commands[2].Id).Error)
	assert.Equal(t, "pending", commands[2].Status)
	assert.Greater(t, commands[2].NextAttemptAt, now)
	assert.Empty(t, commands[2].LeaseToken)
	assert.Zero(t, commands[2].LeaseExpiresAt)
}

func TestImageCleanupPreviewAgeSnapshotProtectionAndDelete(t *testing.T) {
	db, profile := imageCleanupFixture(t)
	image := asyncImageBytesFixture(t)
	now := time.Now().Unix()
	objects := make([]model.ImageStorageObject, 3)
	for i, name := range []string{"old-free", "old-protected", "new-free"} {
		created := now - 3*86400
		if i == 2 {
			created = now
		}
		intent := model.ImageUploadIntent{IntentKey: name, ProfileId: profile.ProfileId, ObjectKey: name + ".png", Class: "temporary", ContentType: image.ContentType, Checksum: image.Checksum, ByteSize: int64(len(image.Data)), Status: "pending", CreatedAt: created}
		var err error
		objects[i], err = StoreImageIntent(t.Context(), intent, image, now-1)
		require.NoError(t, err)
		require.NoError(t, db.Model(&model.ImageUploadIntent{}).Where("intent_key = ?", intent.IntentKey).Update("lease_expires_at", 0).Error)
	}
	require.NoError(t, db.Create(&model.ImageLibraryItem{AssetId: "live", ObjectId: objects[1].ObjectId, SourceKey: "live", ExpiresAt: now + 86400}).Error)
	filters := ImageCleanupFilters{ChannelID: profile.ChannelId, OlderThan: 2, AgeUnit: "days"}
	job, err := NewImageCleanupPreview(t.Context(), "all", filters, 7)
	require.NoError(t, err)
	// Added after preview, even an old backfilled row is outside its snapshot.
	require.NoError(t, db.Create(&model.ImageStorageObject{ObjectId: "late", IdentityHash: "late", ProfileId: profile.ProfileId, Status: "active", CreatedAt: now - 9*86400, ObjectKey: "late.png"}).Error)
	cfg := DefaultImageCleanupConfig()
	require.NoError(t, runImageCleanupSlice(t.Context(), job, cfg, &imageCleanupBudget{}))
	require.NoError(t, db.First(&job, job.Id).Error)
	assert.Equal(t, "preview_ready", job.Status)
	assert.EqualValues(t, 2, job.Scanned)
	assert.EqualValues(t, 1, job.Matched)
	assert.EqualValues(t, 1, job.Protected)
	assert.EqualValues(t, 1, job.ReferenceSkipped)
	_, err = ConfirmImageCleanupPreview(t.Context(), "all", filters, job.PreviewToken, 8)
	require.Error(t, err)
	queued, err := ConfirmImageCleanupPreview(t.Context(), "all", filters, job.PreviewToken, 7)
	require.NoError(t, err)
	again, err := ConfirmImageCleanupPreview(t.Context(), "all", filters, job.PreviewToken, 7)
	require.NoError(t, err)
	assert.Equal(t, queued.JobId, again.JobId)
	require.NoError(t, runImageCleanupSlice(t.Context(), queued, cfg, &imageCleanupBudget{}))
	require.NoError(t, db.First(&job, job.Id).Error)
	assert.Equal(t, "completed", job.Status)
	assert.EqualValues(t, 1, job.Deleted)
	assert.EqualValues(t, 1, job.Skipped)
	assert.EqualValues(t, 1, job.ReferenceSkipped)
	assert.EqualValues(t, len(image.Data), job.DeletedBytes)
	var current model.ImageStorageObject
	require.NoError(t, db.Where("object_id = ?", objects[1].ObjectId).Take(&current).Error)
	assert.Equal(t, "active", current.Status)
	store, err := OpenImageStorage(t.Context(), profile)
	require.NoError(t, err)
	_, err = store.Read(t.Context(), objects[0].ObjectKey, 1024)
	assert.ErrorIs(t, err, ErrImageObjectMissing)
	_, err = store.Read(t.Context(), objects[2].ObjectKey, 1024)
	require.NoError(t, err)
}

func TestImageCleanupConfigurationAndSchedule(t *testing.T) {
	db, profile := imageCleanupFixture(t)
	cfg := DefaultImageCleanupConfig()
	cfg.Enabled = true
	cfg.Policies = []ImageCleanupPolicy{{ID: "daily", Name: "Daily cleanup", Enabled: true, ChannelID: profile.ChannelId, OlderThan: 180, AgeUnit: "days", ScheduleType: "daily", DailyTime: "03:00", Timezone: "Asia/Shanghai"}}
	require.NoError(t, SaveImageCleanupConfig(t.Context(), cfg))
	assert.ErrorIs(t, SaveImageCleanupConfig(t.Context(), cfg), model.ErrOptionRevisionConflict)
	saved, err := GetImageCleanupConfig(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 1, saved.Revision)
	location, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	now := time.Date(2026, 11, 14, 3, 0, 0, 0, location)
	require.NoError(t, scheduleImageCleanup(t.Context(), saved, now.Add(-time.Second)))
	var count int64
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Count(&count).Error)
	assert.Zero(t, count)
	for range 2 {
		require.NoError(t, scheduleImageCleanup(t.Context(), saved, now))
	}
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	var job model.ImageCleanupJob
	require.NoError(t, db.First(&job).Error)
	assert.Equal(t, now.Unix()-180*86400, job.CutoffAt)
	assert.Equal(t, "scheduled", job.Source)
	var snapshot struct {
		PolicySnapshot ImageCleanupPolicy `json:"policy_snapshot"`
	}
	require.NoError(t, common.UnmarshalJsonStr(job.Filters, &snapshot))
	assert.Equal(t, saved.Policies[0].Name, snapshot.PolicySnapshot.Name)
	assert.Equal(t, "Asia/Shanghai", snapshot.PolicySnapshot.Timezone)
	assert.Equal(t, "03:00", snapshot.PolicySnapshot.DailyTime)
	paused, err := ControlImageCleanup(t.Context(), job.JobId, "pause")
	require.NoError(t, err)
	assert.Equal(t, "paused", paused.Status)
	require.NoError(t, scheduleImageCleanup(t.Context(), saved, now.Add(24*time.Hour)))
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Count(&count).Error)
	assert.EqualValues(t, 1, count, "paused scheduled work must not accumulate overlapping runs")
	resumed, err := ControlImageCleanup(t.Context(), job.JobId, "resume")
	require.NoError(t, err)
	assert.Equal(t, "queued", resumed.Status)
	cancelled, err := ControlImageCleanup(t.Context(), job.JobId, "cancel")
	require.NoError(t, err)
	assert.Equal(t, "cancelled", cancelled.Status)
	_, err = ControlImageCleanup(t.Context(), job.JobId, "resume")
	assert.Error(t, err)
	duplicate := saved
	duplicate.Policies = append(append([]ImageCleanupPolicy{}, saved.Policies...), saved.Policies[0])
	duplicate.Policies[1].ID = "other"
	assert.Error(t, duplicate.Validate())
	for _, bad := range []ImageCleanupFilters{{OlderThan: -1, AgeUnit: "hours"}, {OlderThan: 1, AgeUnit: "weeks"}, {OlderThan: 36501, AgeUnit: "days"}} {
		_, err := bad.AgeSeconds()
		assert.Error(t, err)
	}
	seconds, err := (ImageCleanupFilters{OlderThan: 48, AgeUnit: "hours"}).AgeSeconds()
	require.NoError(t, err)
	assert.EqualValues(t, 2*86400, seconds)
}

func TestImageCleanupLegacyLocationAndDurableRetry(t *testing.T) {
	db, profile := imageCleanupFixture(t)
	object := model.ImageStorageObject{ObjectId: "shared", IdentityHash: "shared", ProfileId: profile.ProfileId, ObjectKey: "old.png", Status: "active", CreatedAt: 1}
	require.NoError(t, db.Create(&object).Error)
	peer := profile
	peer.Id, peer.ProfileId, peer.ChannelId, peer.Active = 0, "old-profile", "old-profile", false
	require.NoError(t, db.Create(&peer).Error)
	assert.ErrorIs(t, EnsureImageObjectExclusive(t.Context(), object), errImageSharedLocation, "overlapping legacy roots are protected even before a duplicate object row exists")
	job := model.ImageCleanupJob{JobId: "retry-job", Scope: "all", Filters: "{}", Status: "running", LeaseToken: "lease", LeaseExpiresAt: time.Now().Unix() + 120, CreatedAt: time.Now().Unix()}
	require.NoError(t, db.Create(&job).Error)
	entry := model.ImageCleanupRetry{JobId: job.JobId, ObjectId: object.ObjectId}
	require.NoError(t, db.Create(&entry).Error)
	require.NoError(t, recordImageCleanupRetry(t.Context(), job, entry, context.DeadlineExceeded))
	require.NoError(t, db.First(&entry, entry.Id).Error)
	assert.Zero(t, entry.Attempts, "shutdown/cancellation is not a consumed retry")
	require.NoError(t, recordImageCleanupRetry(t.Context(), job, entry, errors.New("storage unavailable")))
	require.NoError(t, db.First(&entry, entry.Id).Error)
	assert.Equal(t, 1, entry.Attempts)
	assert.Equal(t, "retry", entry.Outcome)
	assert.Greater(t, entry.NextAttemptAt, time.Now().Unix())
	for range 2 {
		require.NoError(t, finishImageCleanupObject(t.Context(), job, entry, object, "lease", "skipped"))
	}
	require.NoError(t, db.First(&job, job.Id).Error)
	assert.EqualValues(t, 1, job.Skipped, "replaying a page does not count protected objects twice")
	assert.EqualValues(t, 1, job.Processed)
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Where("id = ?", job.Id).Update("lease_token", "replacement").Error)
	assert.ErrorIs(t, recordImageCleanupRetry(t.Context(), job, entry, errors.New("late response")), model.ErrImageConflict, "a former runner cannot overwrite replacement retry state")
}

func TestImageCleanupCancelledUncertainDeleteReconciliation(t *testing.T) {
	db, profile := imageCleanupFixture(t)
	now := time.Now().Unix()
	job := model.ImageCleanupJob{JobId: "cancel-unknown", Scope: "all", Filters: "{}", Status: "cancelled", Control: "cancel", CreatedAt: now, FinishedAt: now}
	require.NoError(t, db.Create(&job).Error)
	object := model.ImageStorageObject{ObjectId: "missing-after-cancel", IdentityHash: "missing-after-cancel", ProfileId: profile.ProfileId, ObjectKey: "missing.png", Status: "deleting", DeleteLease: "old-lease", DeleteLeaseExpiresAt: now - 601, ByteSize: 123}
	require.NoError(t, db.Create(&object).Error)
	require.NoError(t, db.Create(&model.ImageCleanupRetry{JobId: job.JobId, ObjectId: object.ObjectId, Outcome: "retry"}).Error)
	require.NoError(t, reconcileStoppedImageCleanup(t.Context(), DefaultImageCleanupConfig(), &imageCleanupBudget{}))
	require.NoError(t, db.First(&object, object.Id).Error)
	assert.Equal(t, "deleted", object.Status)
	require.NoError(t, db.First(&job, job.Id).Error)
	assert.Equal(t, "cancelled", job.Status, "confirming an in-flight delete does not resume cancelled work")
	assert.EqualValues(t, 1, job.Deleted)
	assert.EqualValues(t, 123, job.DeletedBytes)
	require.NoError(t, reconcileStoppedImageCleanup(t.Context(), DefaultImageCleanupConfig(), &imageCleanupBudget{}))
	require.NoError(t, db.First(&job, job.Id).Error)
	assert.EqualValues(t, 1, job.Deleted)
}

func TestImageCleanupPressureHysteresisAndAgeBoundary(t *testing.T) {
	cfg := DefaultImageCleanupConfig()
	guard := imageCleanupLoadGuard{}
	assert.Empty(t, guard.observePressure(cfg, 70, 30))
	assert.NotEmpty(t, guard.observePressure(cfg, 70, 30))
	assert.NotEmpty(t, guard.observePressure(cfg, 59, 69))
	assert.NotEmpty(t, guard.observePressure(cfg, 59, 69))
	assert.Empty(t, guard.observePressure(cfg, 59, 69))
	assert.Empty(t, guard.observePressure(cfg, 10, 80))
	assert.NotEmpty(t, guard.observePressure(cfg, 10, 80))
	assert.NotEmpty(t, guard.observePressure(cfg, math.NaN(), 10))
	assert.NotEmpty(t, guard.observePressure(cfg, 10, math.Inf(1)))
	db, profile := imageCleanupFixture(t)
	boundary := time.Now().Unix() - 180*86400
	for i, created := range []int64{boundary - 1, boundary, boundary + 1} {
		id := []string{"before", "at", "after"}[i]
		require.NoError(t, db.Create(&model.ImageStorageObject{ObjectId: id, IdentityHash: id, ProfileId: profile.ProfileId, ObjectKey: id, Status: "active", CreatedAt: created}).Error)
	}
	query, err := imageCleanupScope(t.Context(), "all", ImageCleanupFilters{OlderThan: 180, AgeUnit: "days", CutoffAt: boundary})
	require.NoError(t, err)
	var count int64
	require.NoError(t, query.Count(&count).Error)
	assert.EqualValues(t, 1, count, "age boundary is strict")
	query, err = imageCleanupScope(t.Context(), "all", ImageCleanupFilters{OlderThan: 36500, AgeUnit: "days", CutoffAt: time.Now().Unix() - 36500*86400})
	require.NoError(t, err)
	require.NoError(t, query.Count(&count).Error)
	assert.Zero(t, count, "a cutoff before Unix epoch must not remove the age condition")
}

func TestImageCleanupContainerLimitsAndScheduleClock(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		files                  map[string]string
		memory, seconds, cores float64
		wantError              bool
	}{
		{name: "cgroup v2", files: map[string]string{"/.dockerenv": "", "/sys/fs/cgroup/memory.current": "80", "/sys/fs/cgroup/memory.max": "100", "/sys/fs/cgroup/cpu.max": "100000 100000", "/sys/fs/cgroup/cpu.stat": "usage_usec 1000000\n"}, memory: 80, seconds: 1, cores: 1},
		{name: "cgroup v1", files: map[string]string{"/.dockerenv": "", "/sys/fs/cgroup/memory/memory.usage_in_bytes": "50", "/sys/fs/cgroup/memory/memory.limit_in_bytes": "100", "/sys/fs/cgroup/cpu/cpu.cfs_quota_us": "200000", "/sys/fs/cgroup/cpu/cpu.cfs_period_us": "100000", "/sys/fs/cgroup/cpuacct/cpuacct.usage": "2000000000"}, memory: 50, seconds: 2, cores: 2},
		{name: "missing container limits", files: map[string]string{"/.dockerenv": ""}, wantError: true},
		{name: "invalid container counter", files: map[string]string{"/.dockerenv": "", "/sys/fs/cgroup/memory.current": "NaN", "/sys/fs/cgroup/memory.max": "100"}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sample, err := readImageCleanupContainerSample(func(path string) ([]byte, error) {
				if value, ok := tc.files[path]; ok {
					return []byte(value), nil
				}
				return nil, os.ErrNotExist
			})
			if tc.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.memory, sample.MemoryPercent)
			assert.Equal(t, tc.seconds, sample.CPUSeconds)
			assert.Equal(t, tc.cores, sample.CPUCores)
		})
	}
	location, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	policy := ImageCleanupPolicy{ScheduleType: "daily", DailyTime: "03:00", Timezone: "Asia/Shanghai"}
	due := time.Date(2026, 11, 14, 3, 0, 0, 0, location)
	slot, next := imageCleanupPolicyWindow(policy, due.Add(-time.Second))
	assert.Zero(t, slot)
	assert.Equal(t, due.Unix(), next)
	slot, next = imageCleanupPolicyWindow(policy, due)
	assert.Equal(t, due.Unix(), slot)
	assert.Equal(t, due.AddDate(0, 0, 1).Unix(), next)
}

func TestImageCleanupProtectsConfirmedUploadBeforeManifest(t *testing.T) {
	db, profile := imageCleanupFixture(t)
	var task model.AsyncImageTask
	require.NoError(t, db.First(&task).Error)
	require.NoError(t, db.Model(&task).Updates(map[string]any{"status": model.ImageTaskUploading, "expires_at": 0}).Error)
	object := model.ImageStorageObject{ObjectId: "unregistered-output", IdentityHash: "unregistered-output", ProfileId: profile.ProfileId, ObjectKey: "output.png", Status: "active", CreatedAt: 1}
	require.NoError(t, db.Create(&object).Error)
	require.NoError(t, db.Create(&model.ImageUploadIntent{IntentKey: "confirmed-output", TaskId: task.TaskId, ProfileId: profile.ProfileId, ObjectKey: object.ObjectKey, Status: "confirmed", CreatedAt: 1}).Error)
	query, err := ImageCleanupCandidates(t.Context(), "all", ImageCleanupFilters{})
	require.NoError(t, err)
	var count int64
	require.NoError(t, query.Where("object_id = ?", object.ObjectId).Count(&count).Error)
	assert.Zero(t, count)
	_, err = model.ClaimImageObjectDeletion(t.Context(), object.ObjectId, "cleaner", time.Now().Unix(), 120)
	assert.ErrorIs(t, err, model.ErrImageConflict)
	require.NoError(t, db.Model(&task).Updates(map[string]any{"status": model.ImageTaskSucceeded, "expires_at": time.Now().Unix() - 1}).Error)
	_, err = model.ClaimImageObjectDeletion(t.Context(), object.ObjectId, "cleaner", time.Now().Unix(), 120)
	require.NoError(t, err)
}

func TestImageCleanupSliceSharesRetryAndFreshDispatchBudget(t *testing.T) {
	db, profile := imageCleanupFixture(t)
	job := model.ImageCleanupJob{JobId: "bounded-slice", Scope: "all", Filters: "{}", Status: "queued", Source: "manual", CreatedAt: time.Now().Unix()}
	objects := make([]model.ImageStorageObject, 160)
	for i := range objects {
		id := fmt.Sprintf("retained-%03d", i)
		objects[i] = model.ImageStorageObject{ObjectId: id, IdentityHash: id, ProfileId: profile.ProfileId, ObjectKey: id, Status: "active", CreatedAt: 1, ExpiresAt: time.Now().Unix() + 86400}
	}
	require.NoError(t, db.CreateInBatches(&objects, 40).Error)
	job.MaxObjectID = objects[len(objects)-1].Id
	require.NoError(t, db.Create(&job).Error)
	retries := make([]model.ImageCleanupRetry, 80)
	for i := range retries {
		retries[i] = model.ImageCleanupRetry{JobId: job.JobId, ObjectId: objects[i].ObjectId, Outcome: "retry"}
	}
	require.NoError(t, db.Create(&retries).Error)
	cfg := DefaultImageCleanupConfig()
	cfg.BatchSize = 200
	require.NoError(t, runImageCleanupSlice(t.Context(), job, cfg, &imageCleanupBudget{}))
	require.NoError(t, db.First(&job, job.Id).Error)
	assert.Greater(t, job.Processed, int64(0))
	assert.LessOrEqual(t, job.Processed, int64(100), "retries and fresh work share the per-slice dispatch cap")
	assert.Equal(t, job.Processed, job.RetentionSkipped)
	assert.Equal(t, "queued", job.Status)
	var details int64
	require.NoError(t, db.Model(&model.ImageCleanupRetry{}).Where("job_id = ?", job.JobId).Count(&details).Error)
	assert.LessOrEqual(t, details, int64(100))
	assert.Zero(t, GetImageCleanupRuntimeStatus().ActiveWorkers)
}

func TestImageCleanupPreviewBudgetPreservesCursor(t *testing.T) {
	db, profile := imageCleanupFixture(t)
	object := model.ImageStorageObject{ObjectId: "preview-later", IdentityHash: "preview-later", ProfileId: profile.ProfileId, ObjectKey: "preview-later", Status: "active", CreatedAt: 1}
	require.NoError(t, db.Create(&object).Error)
	job := model.ImageCleanupJob{JobId: "preview-budget", Scope: "all", Filters: "{}", Status: "previewing", Source: "manual", LeaseToken: "preview-lease", LeaseExpiresAt: time.Now().Unix() + 120, MaxObjectID: object.Id, UserId: 7}
	require.NoError(t, db.Create(&job).Error)
	require.NoError(t, previewImageCleanupSlice(t.Context(), job, job.LeaseToken, ImageCleanupFilters{}, []model.ImageStorageObject{object}, DefaultImageCleanupConfig(), time.Now().Add(-time.Second)))
	require.NoError(t, db.First(&job, job.Id).Error)
	assert.Zero(t, job.Cursor)
	assert.Zero(t, job.Scanned)
	assert.Equal(t, "previewing", job.Status)
	assert.NotEmpty(t, job.LastError)
	require.NoError(t, previewImageCleanupSlice(t.Context(), job, job.LeaseToken, ImageCleanupFilters{}, []model.ImageStorageObject{object}, DefaultImageCleanupConfig(), time.Now().Add(time.Minute)))
	require.NoError(t, db.First(&job, job.Id).Error)
	assert.Equal(t, object.Id, job.Cursor)
	assert.Equal(t, "preview_ready", job.Status)
	assert.EqualValues(t, 1, job.Scanned)
	assert.Greater(t, job.FinishedAt, int64(0), "a long-running preview starts its history retention period when it becomes ready")
}

func TestImageCleanupManyRevisionsRequireCompleteLocationProof(t *testing.T) {
	db, profile := imageCleanupFixture(t)
	profiles := make([]model.ImageStorageProfile, 505)
	for i := range profiles {
		id := fmt.Sprintf("revision-%03d", i)
		profiles[i] = model.ImageStorageProfile{ProfileId: id, ChannelId: id, Class: "temporary", Backend: "local", Root: profile.Root, Namespace: id}
	}
	require.NoError(t, db.CreateInBatches(&profiles, 50).Error)
	last := profiles[len(profiles)-1]
	object := model.ImageStorageObject{ObjectId: "many-revisions", IdentityHash: "many-revisions", ProfileId: last.ProfileId, ObjectKey: "result.png", Status: "active", CreatedAt: 1}
	require.NoError(t, db.Create(&object).Error)
	require.NoError(t, EnsureImageObjectExclusive(t.Context(), object), "more than 500 unrelated revisions must not silently block cleanup")
	peer := last
	peer.Id = 0
	peer.ProfileId = "late-alias"
	peer.ChannelId = "late-alias"
	require.NoError(t, db.Create(&peer).Error)
	require.NoError(t, db.Create(&model.ImageStorageObject{ObjectId: "alias", IdentityHash: "alias", ProfileId: peer.ProfileId, ObjectKey: object.ObjectKey, Status: "active", CreatedAt: 1}).Error)
	assert.ErrorIs(t, EnsureImageObjectExclusive(t.Context(), object), errImageSharedLocation, "a physical alias on a later page must still protect the file")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, EnsureImageObjectExclusive(cancelled, object), context.Canceled)
}

func TestImageCleanupInvalidCredentialsPauseBeforeObjectClaim(t *testing.T) {
	db, _ := imageCleanupFixture(t)
	profile := model.ImageStorageProfile{ProfileId: "invalid-credentials", ChannelId: "invalid-credentials", Namespace: "invalid-credentials", Class: "temporary", Backend: "s3", Provider: "aws", Region: "us-east-1", Bucket: "cleanup-test", CredentialCipher: []byte("credential-canary")}
	require.NoError(t, db.Create(&profile).Error)
	object := model.ImageStorageObject{ObjectId: "configuration-output", IdentityHash: "configuration-output", ProfileId: profile.ProfileId, ObjectKey: "result.png", Status: "active", CreatedAt: 1}
	require.NoError(t, db.Create(&object).Error)
	job := model.ImageCleanupJob{JobId: "invalid-config-job", Scope: "all", Filters: "{}", Status: "queued", Source: "manual", MaxObjectID: object.Id}
	require.NoError(t, db.Create(&job).Error)
	require.NoError(t, runImageCleanupSlice(t.Context(), job, DefaultImageCleanupConfig(), &imageCleanupBudget{}))
	require.NoError(t, db.First(&job, job.Id).Error)
	assert.Equal(t, "paused", job.Status)
	assert.NotContains(t, job.LastError, "credential-canary")
	require.NoError(t, db.First(&object, object.Id).Error)
	assert.Equal(t, "active", object.Status, "invalid configuration must not hide otherwise readable objects behind a deletion lease")
}

func TestImageCleanupHistoryRetainsLiveWorkAndRecentCompletion(t *testing.T) {
	db, profile := imageCleanupFixture(t)
	now := time.Now().Unix()
	old := now - 2*86400
	jobs := []model.ImageCleanupJob{
		{JobId: "history-completed", Status: "completed", CreatedAt: old, FinishedAt: now - 86400},
		{JobId: "history-errors", Status: "completed_with_errors", CreatedAt: old, FinishedAt: now - 86401},
		{JobId: "history-cancelled", Status: "cancelled", CreatedAt: old, FinishedAt: now - 86401},
		{JobId: "history-legacy-failed", Status: "failed", CreatedAt: old},
		{JobId: "history-preview", Status: "preview_ready", CreatedAt: now - 86400},
		{JobId: "history-recent-preview", Status: "preview_ready", CreatedAt: now - 86399},
		{JobId: "history-long-preview", Status: "preview_ready", CreatedAt: old, FinishedAt: now},
		{JobId: "history-recent-finish", Status: "completed", CreatedAt: old, FinishedAt: now - 86399},
		{JobId: "history-live-lease", Status: "failed", CreatedAt: old, FinishedAt: old, LeaseToken: "worker", LeaseExpiresAt: now + 60},
		{JobId: "history-paused", Status: "paused", CreatedAt: old},
		{JobId: "history-queued", Status: "queued", CreatedAt: old},
		{JobId: "history-running", Status: "running", CreatedAt: old},
		{JobId: "history-previewing", Status: "previewing", CreatedAt: old},
		{JobId: "history-uncertain", Status: "cancelled", CreatedAt: old, FinishedAt: old},
	}
	require.NoError(t, db.Create(&jobs).Error)
	uncertain := model.ImageStorageObject{ObjectId: "history-uncertain-object", IdentityHash: "history-uncertain-object", ProfileId: profile.ProfileId, ObjectKey: "history-uncertain.png", Status: "deleting", DeleteLease: "old-worker", DeleteLeaseExpiresAt: old}
	require.NoError(t, db.Create(&uncertain).Error)
	entries := []model.ImageCleanupRetry{
		{JobId: "history-errors", ObjectId: "history-old-failure", Outcome: "retry", Attempts: 3, LastError: "storage unavailable"},
		{JobId: "history-uncertain", ObjectId: uncertain.ObjectId, Outcome: "retry"},
		{JobId: "history-paused", ObjectId: "history-paused-object", Outcome: "retry"},
	}
	require.NoError(t, db.Create(&entries).Error)
	require.NoError(t, model.PruneImageCleanupHistory(t.Context(), now))
	var remaining []string
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Order("id").Pluck("job_id", &remaining).Error)
	assert.Equal(t, []string{"history-recent-preview", "history-long-preview", "history-recent-finish", "history-live-lease", "history-paused", "history-queued", "history-running", "history-previewing", "history-uncertain"}, remaining)
	var details []string
	require.NoError(t, db.Model(&model.ImageCleanupRetry{}).Order("id").Pluck("job_id", &details).Error)
	assert.Equal(t, []string{"history-uncertain", "history-paused"}, details)
	// Only after the actual storage-presence check confirms a stopped deletion
	// may the retained recovery record and its expired summary disappear.
	require.NoError(t, reconcileStoppedImageCleanup(t.Context(), DefaultImageCleanupConfig(), &imageCleanupBudget{}))
	require.NoError(t, db.First(&uncertain, uncertain.Id).Error)
	assert.Equal(t, "deleted", uncertain.Status)
	require.NoError(t, model.PruneImageCleanupHistory(t.Context(), now))
	var count int64
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Where("job_id = ?", "history-uncertain").Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, db.Model(&model.ImageCleanupRetry{}).Where("job_id = ?", "history-uncertain").Count(&count).Error)
	assert.Zero(t, count)
}

func TestImageCleanupHistoryShowsOnlyFiveNewestVisibleJobs(t *testing.T) {
	db, _ := imageCleanupFixture(t)
	now := time.Now().Unix()
	jobs := []model.ImageCleanupJob{
		{JobId: "expired-summary", Status: "completed", CreatedAt: now - 90000, FinishedAt: now - 86400},
		{JobId: "expired-preview", Status: "preview_ready", CreatedAt: now - 86400},
		{JobId: "active-old", Status: "paused", CreatedAt: now - 3*86400},
		{JobId: "recent-one", Status: "completed", CreatedAt: now - 6, FinishedAt: now - 1},
		{JobId: "recent-two", Status: "completed", CreatedAt: now - 5, FinishedAt: now - 1},
		{JobId: "recent-three", Status: "completed", CreatedAt: now - 4, FinishedAt: now - 1},
		{JobId: "recent-four", Status: "completed", CreatedAt: now - 3, FinishedAt: now - 1},
		{JobId: "recent-five", Status: "completed", CreatedAt: now - 2, FinishedAt: now - 1},
		{JobId: "recent-six", Status: "completed", CreatedAt: now - 2, FinishedAt: now - 1},
	}
	require.NoError(t, db.Create(&jobs).Error)
	visible, err := model.RecentImageCleanupJobs(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, visible, 5)
	ids := make([]string, len(visible))
	for i, job := range visible {
		ids[i] = job.JobId
	}
	assert.Equal(t, []string{"recent-six", "recent-five", "recent-four", "recent-three", "recent-two"}, ids)
	require.NoError(t, db.Where("job_id IN ?", ids).Delete(&model.ImageCleanupJob{}).Error)
	visible, err = model.RecentImageCleanupJobs(t.Context(), now)
	require.NoError(t, err)
	require.Len(t, visible, 2, "expired records are hidden before the rolling maintenance pass")
	assert.Equal(t, "recent-one", visible[0].JobId)
	assert.Equal(t, "active-old", visible[1].JobId)
	// Actual legacy SQL NULLs differ from Go's zero-valued integer fields:
	// NOT(NULL) must not hide a recent terminal job before its 24-hour expiry.
	legacy := []model.ImageCleanupJob{
		{JobId: "null-recent-completed", Status: "completed", CreatedAt: now - 3600},
		{JobId: "null-expired-failed", Status: "failed", CreatedAt: now - 86400},
		{JobId: "null-unfinished", Status: "running", CreatedAt: now - 4*86400},
	}
	require.NoError(t, db.Create(&legacy).Error)
	legacyIDs := []string{legacy[0].JobId, legacy[1].JobId, legacy[2].JobId}
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Where("job_id IN ?", legacyIDs).UpdateColumns(map[string]any{"finished_at": nil, "lease_token": nil, "lease_expires_at": nil}).Error)
	var count int64
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Where("job_id IN ? AND finished_at IS NULL AND lease_expires_at IS NULL AND lease_token IS NULL", legacyIDs).Count(&count).Error)
	require.EqualValues(t, 3, count)
	visible, err = model.RecentImageCleanupJobs(t.Context(), now)
	require.NoError(t, err)
	ids = ids[:0]
	for _, job := range visible {
		ids = append(ids, job.JobId)
	}
	assert.Contains(t, ids, "null-recent-completed")
	assert.Contains(t, ids, "null-unfinished")
	assert.NotContains(t, ids, "null-expired-failed")
	require.NoError(t, model.PruneImageCleanupHistory(t.Context(), now))
	var preserved []string
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Where("job_id IN ?", legacyIDs).Order("id").Pluck("job_id", &preserved).Error)
	assert.Equal(t, []string{"null-recent-completed", "null-unfinished"}, preserved)
	for _, job := range jobs {
		legacyIDs = append(legacyIDs, job.JobId)
	}
	require.NoError(t, db.Where("job_id IN ?", legacyIDs).Delete(&model.ImageCleanupJob{}).Error)
	// Unknown legacy status/age is retained conservatively. A known completion
	// time remains sufficient even when the original creation time is absent.
	for _, tc := range []struct {
		id      string
		updates map[string]any
	}{
		{"unknown-status", map[string]any{"status": nil, "created_at": now - 2*86400, "finished_at": now - 86400}},
		{"unknown-null-age", map[string]any{"created_at": nil, "finished_at": nil}},
		{"unknown-zero-age", map[string]any{"created_at": 0, "finished_at": 0}},
		{"known-old-finish", map[string]any{"created_at": nil, "finished_at": now - 86400}},
		{"known-recent-finish", map[string]any{"created_at": nil, "finished_at": now - 3600}},
	} {
		job := model.ImageCleanupJob{JobId: tc.id, Status: "failed", CreatedAt: now}
		require.NoError(t, db.Create(&job).Error)
		require.NoError(t, db.Model(&job).UpdateColumns(tc.updates).Error)
	}
	visible, err = model.RecentImageCleanupJobs(t.Context(), now)
	require.NoError(t, err)
	ids = ids[:0]
	for _, job := range visible {
		ids = append(ids, job.JobId)
	}
	expected := []string{"unknown-status", "unknown-null-age", "unknown-zero-age", "known-recent-finish"}
	assert.ElementsMatch(t, expected, ids)
	require.NoError(t, model.PruneImageCleanupHistory(t.Context(), now))
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Pluck("job_id", &preserved).Error)
	assert.ElementsMatch(t, expected, preserved)
}

func TestImageCleanupHistoryPrunesDetailsAndSummariesInBoundedBatches(t *testing.T) {
	db, _ := imageCleanupFixture(t)
	now := time.Now().Unix()
	job := model.ImageCleanupJob{JobId: "large-history", Status: "completed", CreatedAt: now - 2*86400, FinishedAt: now - 86401}
	require.NoError(t, db.Create(&job).Error)
	entries := make([]model.ImageCleanupRetry, 201)
	for i := range entries {
		entries[i] = model.ImageCleanupRetry{JobId: job.JobId, ObjectId: fmt.Sprintf("history-detail-%03d", i), Outcome: "deleted"}
	}
	require.NoError(t, db.CreateInBatches(&entries, 50).Error)
	require.NoError(t, model.PruneImageCleanupHistory(t.Context(), now))
	var count int64
	require.NoError(t, db.Model(&model.ImageCleanupRetry{}).Count(&count).Error)
	assert.EqualValues(t, 1, count, "one pass removes at most 200 detail rows")
	require.NoError(t, db.First(&job, job.Id).Error, "the parent survives while any detail rows remain")
	require.NoError(t, model.PruneImageCleanupHistory(t.Context(), now))
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Count(&count).Error)
	assert.Zero(t, count)
	require.NoError(t, db.Model(&model.ImageCleanupRetry{}).Count(&count).Error)
	assert.Zero(t, count)
	jobs := make([]model.ImageCleanupJob, 21)
	for i := range jobs {
		jobs[i] = model.ImageCleanupJob{JobId: fmt.Sprintf("summary-%02d", i), Status: "failed", CreatedAt: now - 2*86400}
	}
	require.NoError(t, db.Create(&jobs).Error)
	require.NoError(t, model.PruneImageCleanupHistory(t.Context(), now))
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Count(&count).Error)
	assert.EqualValues(t, 1, count, "one pass removes at most 20 summaries")
	require.NoError(t, model.PruneImageCleanupHistory(t.Context(), now))
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestImageCleanupHistoryMaintenanceSurvivesStorageConfigurationFailure(t *testing.T) {
	db, _ := imageCleanupFixture(t)
	job := model.ImageCleanupJob{JobId: "history-before-storage-failure", Status: "completed", CreatedAt: time.Now().Unix() - 2*86400, FinishedAt: time.Now().Unix() - 86401}
	require.NoError(t, db.Create(&job).Error)
	require.NoError(t, db.Model(&model.Option{}).Where(map[string]any{"key": "image_runtime_config"}).Update("value", "invalid config").Error)
	_, err := GetImageRuntimeConfig(t.Context())
	require.Error(t, err)
	require.Error(t, MaintainImageCleanup(t.Context()), "the existing storage-maintenance failure remains visible")
	var count int64
	require.NoError(t, db.Model(&model.ImageCleanupJob{}).Where("job_id = ?", job.JobId).Count(&count).Error)
	assert.Zero(t, count, "storage maintenance must not prevent the bounded history pass")
}

func TestImageCleanupScheduledPolicyDeletesRetainedResults(t *testing.T) {
	for _, backend := range []string{"local", "r2"} {
		t.Run(backend, func(t *testing.T) {
			db, profile := imageCleanupFixture(t)
			var deletes atomic.Int32
			var server *httptest.Server
			if backend == "r2" {
				var mu sync.Mutex
				files := map[string][]byte{}
				server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					if r.URL.Query().Has("versioning") {
						w.Header().Set("Content-Type", "application/xml")
						_, _ = io.WriteString(w, `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>`)
						return
					}
					switch r.Method {
					case http.MethodPut:
						data, err := io.ReadAll(r.Body)
						if err != nil {
							http.Error(w, "invalid upload", http.StatusBadRequest)
							return
						}
						files[r.URL.Path] = data
					case http.MethodDelete:
						delete(files, r.URL.Path)
						deletes.Add(1)
						w.WriteHeader(http.StatusNoContent)
					case http.MethodGet, http.MethodHead:
						data, ok := files[r.URL.Path]
						if !ok {
							w.WriteHeader(http.StatusNotFound)
							if r.Method == http.MethodGet {
								_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
							}
							return
						}
						w.Header().Set("Content-Type", "image/png")
						w.Header().Set("Content-Length", strconv.Itoa(len(data)))
						if r.Method == http.MethodGet {
							_, _ = w.Write(data)
						}
					default:
						w.WriteHeader(http.StatusMethodNotAllowed)
					}
				}))
				t.Cleanup(server.Close)
				previousTransport := http.DefaultTransport
				http.DefaultTransport = server.Client().Transport
				t.Cleanup(func() { http.DefaultTransport = previousTransport })
				profile = model.ImageStorageProfile{ProfileId: "cleanup-r2", ChannelId: "cleanup-r2-channel", Namespace: "cleanup-r2", CurrentRevision: true, Class: "temporary", Backend: "s3", Provider: "r2", Bucket: "cleanup-images", Region: "auto", Endpoint: server.URL, PublicBaseURL: "https://images.example.test", PathStyle: true}
				data, err := common.Marshal(ImageStorageCredentials{AccessKey: "synthetic-access", SecretKey: "synthetic-secret"})
				require.NoError(t, err)
				profile.CredentialCipher, err = EncryptImagePayload(data, "storage:"+profile.ProfileId)
				require.NoError(t, err)
				require.NoError(t, db.Create(&profile).Error)
			}
			now := time.Now().Unix()
			var task model.AsyncImageTask
			require.NoError(t, db.First(&task).Error)
			require.NoError(t, db.Model(&task).Updates(map[string]any{"status": model.ImageTaskSucceeded, "billing_status": "succeeded", "result_count": 1, "image_count": 1, "quota": 100, "created_at": now - 3*3600, "expires_at": now + 180*86400}).Error)
			require.NoError(t, db.First(&task, task.Id).Error)
			image := asyncImageBytesFixture(t)
			intent := model.ImageUploadIntent{IntentKey: "policy-source", TaskId: task.TaskId, UserId: task.UserId, ProfileId: profile.ProfileId, ObjectKey: "result.png", Class: "temporary", ContentType: image.ContentType, ByteSize: int64(len(image.Data)), Checksum: image.Checksum, Status: "pending", CreatedAt: now - 3*3600}
			object, err := StoreImageIntent(t.Context(), intent, image, now+180*86400)
			require.NoError(t, err)
			require.NoError(t, db.Model(&model.ImageUploadIntent{}).Where("intent_key = ?", intent.IntentKey).Update("lease_expires_at", 0).Error)
			require.NoError(t, db.Create(&model.AsyncImageResult{TaskId: task.TaskId, ObjectId: object.ObjectId, CreatedAt: intent.CreatedAt}).Error)
			store, err := OpenImageStorage(t.Context(), profile)
			require.NoError(t, err)
			var filters ImageCleanupFilters
			require.NoError(t, common.UnmarshalJsonStr(fmt.Sprintf(`{"channel_id":%q,"older_than":2,"age_unit":"hours","ignore_retention":true}`, profile.ChannelId), &filters))
			manual, err := NewImageCleanupPreview(t.Context(), "all", filters, task.UserId)
			require.NoError(t, err)
			cfg := DefaultImageCleanupConfig()
			require.NoError(t, runImageCleanupSlice(t.Context(), manual, cfg, &imageCleanupBudget{}))
			require.NoError(t, db.First(&manual, manual.Id).Error)
			assert.EqualValues(t, 0, manual.Matched, "a request cannot bypass retention by injecting ignore_retention")
			assert.EqualValues(t, 1, manual.RetentionSkipped)
			manual, err = ConfirmImageCleanupPreview(t.Context(), "all", filters, manual.PreviewToken, task.UserId)
			require.NoError(t, err)
			require.NoError(t, runImageCleanupSlice(t.Context(), manual, cfg, &imageCleanupBudget{}))
			require.NoError(t, db.First(&manual, manual.Id).Error)
			assert.EqualValues(t, 0, manual.Deleted)
			assert.EqualValues(t, 1, manual.RetentionSkipped)
			data, err := store.Read(t.Context(), object.ObjectKey, 1024)
			require.NoError(t, err)
			assert.Equal(t, image.Data, data)
			cfg.Enabled = true
			cfg.Policies = []ImageCleanupPolicy{{ID: "two-hours", Name: "Two-hour cleanup", Enabled: true, ChannelID: profile.ChannelId, OlderThan: 2, AgeUnit: "hours", ScheduleType: "interval", IntervalMinutes: 15, Timezone: "Asia/Shanghai"}}
			require.NoError(t, scheduleImageCleanup(t.Context(), cfg, time.Unix(now, 0)))
			var scheduled model.ImageCleanupJob
			require.NoError(t, db.Where("source = ? AND policy_id = ?", "scheduled", "two-hours").Take(&scheduled).Error)
			require.NoError(t, runImageCleanupSlice(t.Context(), scheduled, cfg, &imageCleanupBudget{}))
			require.NoError(t, db.First(&scheduled, scheduled.Id).Error)
			assert.Equal(t, "completed", scheduled.Status)
			assert.EqualValues(t, 1, scheduled.Deleted)
			assert.EqualValues(t, len(image.Data), scheduled.DeletedBytes)
			assert.Zero(t, scheduled.Errors)
			require.NoError(t, db.First(&object, object.Id).Error)
			assert.Equal(t, "deleted", object.Status)
			_, err = store.Read(t.Context(), object.ObjectKey, 1024)
			assert.ErrorIs(t, err, ErrImageObjectMissing, "cleanup must remove physical bytes")
			if server != nil {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodHead, server.URL+"/"+profile.Bucket+"/"+store.Key(object.ObjectKey), nil)
				require.NoError(t, err)
				response, err := server.Client().Do(request)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				assert.Equal(t, http.StatusNotFound, response.StatusCode, "R2 HEAD confirms the object is absent")
			}
			require.NoError(t, runImageCleanupSlice(t.Context(), scheduled, cfg, &imageCleanupBudget{}))
			require.NoError(t, db.First(&scheduled, scheduled.Id).Error)
			assert.EqualValues(t, 1, scheduled.Deleted, "replaying a completed job cannot double-count deletion")
			if backend == "r2" {
				assert.EqualValues(t, 1, deletes.Load())
			}
			var savedTask model.AsyncImageTask
			require.NoError(t, db.First(&savedTask, task.Id).Error)
			assert.Equal(t, task.Status, savedTask.Status)
			assert.Equal(t, task.BillingStatus, savedTask.BillingStatus)
			assert.Equal(t, task.ExpiresAt, savedTask.ExpiresAt)
			assert.Equal(t, task.Quota, savedTask.Quota)
			var user model.User
			var token model.Token
			require.NoError(t, db.First(&user, task.UserId).Error)
			require.NoError(t, db.First(&token, task.TokenId).Error)
			assert.Equal(t, 1000, user.Quota)
			assert.Equal(t, 1000, token.RemainQuota)
		})
	}
}

func TestImageCleanupScheduledPolicyPreservesAgeScopeAndLiveConsumers(t *testing.T) {
	db, profile := imageCleanupFixture(t)
	now := time.Now().Unix()
	cutoff := now - 2*3600
	otherProfile := model.ImageStorageProfile{ProfileId: "other-location", ChannelId: "other-channel", Class: "temporary", Backend: "local", Root: t.TempDir()}
	require.NoError(t, db.Create(&otherProfile).Error)
	filters := ImageCleanupFilters{ChannelID: profile.ChannelId, OlderThan: 2, AgeUnit: "hours", CutoffAt: cutoff, ProfileIDs: []string{profile.ProfileId}, IgnoreRetention: true}
	for _, scenario := range []struct {
		name     string
		eligible bool
	}{
		{name: "retained-completed", eligible: true},
		{name: "new-image"},
		{name: "exact-age-boundary"},
		{name: "other-location"},
		{name: "running-manifest"},
		{name: "running-confirmed-intent"},
		{name: "live-library"},
		{name: "published-deleted-library"},
		{name: "live-input"},
		{name: "input-for-running-task"},
		{name: "uploading-intent"},
		{name: "publication-lease"},
		{name: "pending-archive"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			object := model.ImageStorageObject{ObjectId: scenario.name, IdentityHash: scenario.name, ProfileId: profile.ProfileId, ObjectKey: scenario.name + ".png", Status: "active", Class: "temporary", CreatedAt: cutoff - 1, ExpiresAt: now + 180*86400}
			switch scenario.name {
			case "new-image":
				object.CreatedAt = now
			case "exact-age-boundary":
				object.CreatedAt = cutoff
			case "other-location":
				object.ProfileId = otherProfile.ProfileId
			}
			require.NoError(t, db.Create(&object).Error)
			task := model.AsyncImageTask{TaskId: "task-" + scenario.name, Status: model.ImageTaskSucceeded, BillingStatus: "succeeded", ResultCount: 1, CreatedAt: object.CreatedAt, ExpiresAt: now + 180*86400}
			if scenario.name == "running-manifest" || scenario.name == "running-confirmed-intent" {
				task.Status = model.ImageTaskUploading
			}
			require.NoError(t, db.Create(&task).Error)
			if scenario.name != "running-confirmed-intent" {
				require.NoError(t, db.Create(&model.AsyncImageResult{TaskId: task.TaskId, ObjectId: object.ObjectId}).Error)
			}
			switch scenario.name {
			case "running-confirmed-intent", "uploading-intent", "publication-lease":
				intent := model.ImageUploadIntent{IntentKey: "intent-" + scenario.name, ProfileId: object.ProfileId, ObjectKey: object.ObjectKey, Status: "confirmed"}
				if scenario.name == "running-confirmed-intent" {
					intent.TaskId = task.TaskId
				} else if scenario.name == "uploading-intent" {
					intent.Status = "uploading"
				} else {
					intent.LeaseExpiresAt = now + 600
				}
				require.NoError(t, db.Create(&intent).Error)
			case "live-library", "published-deleted-library":
				item := model.ImageLibraryItem{AssetId: "asset-" + scenario.name, SourceKey: "source-" + scenario.name, ObjectId: object.ObjectId, ExpiresAt: now + 86400}
				if scenario.name == "published-deleted-library" {
					item.DeletedAt, item.ExpiresAt = now, now-1
				}
				require.NoError(t, db.Create(&item).Error)
				if scenario.name == "published-deleted-library" {
					require.NoError(t, db.Create(&model.ImagePublication{PublicationId: "publication", AssetId: item.AssetId, Status: "published", ExpiresAt: now + 86400}).Error)
				}
			case "live-input", "input-for-running-task":
				input := model.ImageInputObject{InputId: "input-" + scenario.name, KeyHash: scenario.name, ObjectId: object.ObjectId, Status: "active", ExpiresAt: now + 86400}
				if scenario.name == "input-for-running-task" {
					input.Status, input.ExpiresAt = "deleted", now-1
				}
				require.NoError(t, db.Create(&input).Error)
				if scenario.name == "input-for-running-task" {
					consumer := model.AsyncImageTask{TaskId: "active-input-consumer", Status: model.ImageTaskInvoking}
					require.NoError(t, db.Create(&consumer).Error)
					require.NoError(t, db.Create(&model.ImageInputTaskReference{TaskId: consumer.TaskId, InputId: input.InputId}).Error)
				}
			case "pending-archive":
				require.NoError(t, db.Create(&model.ImageOutbox{EventKey: "pending-archive", AggregateId: task.TaskId, Kind: "library_archive", Status: "pending"}).Error)
			}
			query, err := ImageCleanupCandidates(t.Context(), "all", filters)
			require.NoError(t, err)
			var count int64
			require.NoError(t, query.Where("object_id = ?", object.ObjectId).Count(&count).Error)
			assert.Equal(t, scenario.eligible, count == 1, "the age policy must preserve scope and active consumers")
			claimed, err := model.ClaimImageObjectPolicyDeletion(t.Context(), object.ObjectId, "policy-cleaner", now, 120, cutoff, filters.ProfileIDs)
			if scenario.eligible {
				require.NoError(t, err)
				assert.Equal(t, "deleting", claimed.Status)
			} else {
				assert.ErrorIs(t, err, model.ErrImageConflict, "the locked deletion check must reject newly protected objects too")
				require.NoError(t, db.First(&object, object.Id).Error)
				assert.Equal(t, "active", object.Status)
			}
		})
	}
}
