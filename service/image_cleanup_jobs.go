package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const imageCleanupSystemTaskType = "image_storage_maintenance"

// NewImageCleanupPreview freezes the object boundary and channel revisions;
// only the bounded background scanner calculates the preview totals.
func NewImageCleanupPreview(ctx context.Context, scope string, filters ImageCleanupFilters, actor int) (model.ImageCleanupJob, error) {
	var pending int64
	if err := model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("user_id = ? AND status = ?", actor, "previewing").Count(&pending).Error; err != nil {
		return model.ImageCleanupJob{}, err
	}
	if pending >= 5 {
		return model.ImageCleanupJob{}, errors.New("wait for an existing cleanup preview before creating another")
	}
	filters.CutoffAt, filters.ProfileIDs = 0, nil
	age, err := filters.AgeSeconds()
	if err != nil {
		return model.ImageCleanupJob{}, err
	}
	if age > 0 {
		filters.CutoffAt = time.Now().Unix() - age
	}
	if filters.ChannelID != "" {
		profiles, err := model.ImageStorageChannelProfiles(ctx, filters.ChannelID)
		if err != nil {
			return model.ImageCleanupJob{}, err
		}
		for _, p := range profiles {
			filters.ProfileIDs = append(filters.ProfileIDs, p.ProfileId)
		}
	}
	if _, err := imageCleanupScope(ctx, scope, filters); err != nil {
		return model.ImageCleanupJob{}, err
	}
	var maxID int
	if err := model.DB.WithContext(ctx).Model(&model.ImageStorageObject{}).Select("COALESCE(MAX(id),0)").Scan(&maxID).Error; err != nil {
		return model.ImageCleanupJob{}, err
	}
	data, err := common.Marshal(filters)
	if err != nil {
		return model.ImageCleanupJob{}, err
	}
	source := "manual"
	if filters.ChannelID != "" && filters.CutoffAt > 0 && len(filters.ProfileIDs) > 0 {
		source = "manual_age"
	}
	job := model.ImageCleanupJob{JobId: "imgclean_" + common.GetUUID(), Scope: scope, Filters: string(data), UserId: actor, Source: source, Status: "previewing", CutoffAt: filters.CutoffAt, MaxObjectID: maxID, CreatedAt: time.Now().Unix()}
	err = model.DB.WithContext(ctx).Create(&job).Error
	return job, err
}

func ConfirmImageCleanupPreview(ctx context.Context, scope string, filters ImageCleanupFilters, token string, actor int) (model.ImageCleanupJob, error) {
	var job model.ImageCleanupJob
	if !ValidateImageCleanupPreview(token, scope, filters, actor, time.Now().Unix()) {
		return job, errors.New("preview the same cleanup scope and filters before creating a job")
	}
	err := model.DB.WithContext(ctx).Where("preview_token = ? AND user_id = ?", token, actor).Take(&job).Error
	if err != nil {
		return job, err
	}
	if job.Status != "preview_ready" {
		return job, nil
	}
	err = model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("id = ? AND status = ?", job.Id, "preview_ready").Updates(map[string]any{"status": "queued", "cursor": 0, "scanned": 0, "skipped": 0, "protected": 0, "reference_skipped": 0, "retention_skipped": 0, "location_skipped": 0, "finished_at": 0, "total": job.Scanned}).Error
	if err == nil {
		err = model.DB.WithContext(ctx).Where("id = ?", job.Id).Take(&job).Error
	}
	return job, err
}

func ControlImageCleanup(ctx context.Context, id, action string) (model.ImageCleanupJob, error) {
	var job model.ImageCleanupJob
	if err := model.DB.WithContext(ctx).Where("job_id = ?", id).Take(&job).Error; err != nil {
		return job, err
	}
	updates := map[string]any{}
	switch action {
	case "pause":
		if job.Status != "queued" && job.Status != "running" {
			return job, errors.New("only an active cleanup can be paused")
		}
		updates["control"], updates["status"] = "pause", "paused"
	case "resume":
		if job.Status != "paused" {
			return job, errors.New("only a paused cleanup can be resumed")
		}
		updates["control"], updates["status"], updates["last_error"], updates["next_attempt_at"] = "", "queued", "", 0
	case "cancel":
		if job.Status == "completed" || job.Status == "completed_with_errors" || job.Status == "cancelled" || job.Status == "failed" {
			return job, nil
		}
		updates["control"], updates["status"], updates["finished_at"] = "cancel", "cancelled", time.Now().Unix()
	default:
		return job, errors.New("invalid image cleanup action")
	}
	err := model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("id = ? AND status = ?", job.Id, job.Status).Updates(updates).Error
	if err == nil {
		err = model.DB.WithContext(ctx).Where("id = ?", job.Id).Take(&job).Error
	}
	return job, err
}

func WakeImageCleanup() {
	_, _, err := EnqueueSystemTask(imageCleanupSystemTaskType, nil)
	if err != nil {
		common.SysError("image cleanup dispatcher wakeup unavailable")
	}
}

type imageCleanupHandler struct{}

func (imageCleanupHandler) Type() string            { return imageCleanupSystemTaskType }
func (imageCleanupHandler) Enabled() bool           { return true }
func (imageCleanupHandler) Interval() time.Duration { return 5 * time.Second }
func (imageCleanupHandler) NewPayload() any         { return nil }
func init()                                         { RegisterSystemTaskHandler(imageCleanupHandler{}) }

func (imageCleanupHandler) Run(parent context.Context, task *model.SystemTask, runner string) {
	err := MaintainImageCleanup(parent)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		failSystemTask(task, runner, err)
		return
	}
	if err := model.FinishSystemTask(task.TaskID, runner, model.SystemTaskStatusSucceeded, nil, ""); err != nil {
		logSystemTaskLockError(parent, task, err)
	}
}

func MaintainImageCleanup(ctx context.Context) error {
	var budget, scanBudget imageCleanupBudget
	for {
		cfg, err := GetImageCleanupConfig(ctx)
		if err != nil {
			return err
		}
		budget.SetRate(cfg.RequestsPerSecond)
		scanBudget.SetRate(cfg.ScanPagesPerSecond)
		// Each pass has its own I/O deadline. Ready backlog continues under the
		// system task's renewable lease instead of waiting for the scheduler.
		pass, cancel := context.WithTimeout(ctx, time.Duration(cfg.RequestTimeoutSeconds+15)*time.Second)
		err = maintainImageCleanupPass(pass, cfg, &budget, &scanBudget)
		passErr := pass.Err()
		cancel()
		if err != nil {
			return err
		}
		if passErr != nil {
			return passErr
		}
		var ready model.ImageCleanupJob
		now := time.Now().Unix()
		result := model.DB.WithContext(ctx).Select("id").Where("status IN ? AND COALESCE(control, '') = ? AND lease_expires_at <= ? AND next_attempt_at <= ?", []string{"previewing", "queued", "running"}, "", now, now).Limit(1).Find(&ready)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
	}
}

func maintainImageCleanupPass(ctx context.Context, cfg ImageCleanupConfig, budget, scanBudget *imageCleanupBudget) error {
	if err := scheduleImageCleanup(ctx, cfg, time.Now()); err != nil {
		return err
	}
	// Retire database history before any remote deletion work can consume the
	// slice's timeout. Its own small budget keeps slow storage from starving it.
	if imageCleanupLoad.Check(cfg) == "" {
		history, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := model.PruneImageCleanupHistory(history, time.Now().Unix())
		cancel()
		if err != nil {
			if ctx.Err() != nil || !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
				return err
			}
			common.SysLog("Image cleanup history reached its time budget; continuing cleanup work")
		}
	}
	ctx = WithImageStorageRequestBudget(ctx, budget.Wait)
	ctx, err := withImageCleanupProfiles(ctx)
	if err != nil {
		return err
	}
	var jobs []model.ImageCleanupJob
	now := time.Now().Unix()
	if err := model.DB.WithContext(ctx).Where("status IN ? AND COALESCE(control, '') = ? AND lease_expires_at <= ? AND next_attempt_at <= ?", []string{"previewing", "queued", "running"}, "", now, now).Order("next_attempt_at,id").Limit(2).Find(&jobs).Error; err != nil {
		return err
	}
	for _, job := range jobs {
		if err := scanBudget.Wait(ctx); err != nil {
			return err
		}
		if err := runImageCleanupSlice(ctx, job, cfg, budget); err != nil {
			return err
		}
	}
	// Legacy intent reaping now shares the independent low-priority worker and
	// request budget, never the generation recovery loop.
	if imageCleanupLoad.Check(cfg) != "" {
		return nil
	}
	if err := reconcileStoppedImageCleanup(ctx, cfg, budget); err != nil {
		return err
	}
	runtime, err := GetImageRuntimeConfig(ctx)
	if err != nil {
		return err
	}
	maintenance, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := MaintainImageStorage(maintenance, runtime); err != nil {
		return err
	}
	var history []int64
	maintenanceTypes := []string{imageCleanupSystemTaskType, "image_library_archive"}
	if err := model.DB.WithContext(ctx).Model(&model.SystemTask{}).Where("type IN ? AND status IN ? AND updated_at < ?", maintenanceTypes, []model.SystemTaskStatus{model.SystemTaskStatusSucceeded, model.SystemTaskStatusFailed}, time.Now().Unix()-7*86400).Order("id").Limit(100).Pluck("id", &history).Error; err != nil {
		return err
	}
	if len(history) > 0 {
		return model.DB.WithContext(ctx).Where("id IN ? AND type IN ?", history, maintenanceTypes).Delete(&model.SystemTask{}).Error
	}
	return nil
}

func scheduleImageCleanup(ctx context.Context, cfg ImageCleanupConfig, now time.Time) error {
	if !cfg.Enabled {
		return nil
	}
	for _, policy := range cfg.Policies {
		if !policy.Enabled {
			continue
		}
		slot, _ := imageCleanupPolicyWindow(policy, now)
		if slot == 0 {
			continue
		}
		var active int64
		if err := model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("policy_id = ? AND status IN ?", policy.ID, []string{"queued", "running", "paused"}).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			continue
		}
		filters := ImageCleanupFilters{ChannelID: policy.ChannelID, OlderThan: policy.OlderThan, AgeUnit: policy.AgeUnit}
		age, _ := filters.AgeSeconds()
		filters.CutoffAt = now.Unix() - age
		profiles, err := model.ImageStorageChannelProfiles(ctx, policy.ChannelID)
		if err != nil {
			return err
		}
		for _, p := range profiles {
			filters.ProfileIDs = append(filters.ProfileIDs, p.ProfileId)
		}
		data, err := common.Marshal(struct {
			ImageCleanupFilters
			PolicySnapshot ImageCleanupPolicy `json:"policy_snapshot"`
		}{filters, policy})
		if err != nil {
			return err
		}
		err = model.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			guard := model.ImageCleanupSchedule{PolicyId: policy.ID}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&guard).Error; err != nil {
				return err
			}
			claim := tx.Model(&model.ImageCleanupSchedule{}).Where("policy_id = ? AND slot < ?", policy.ID, slot).Update("slot", slot)
			if claim.Error != nil || claim.RowsAffected == 0 {
				return claim.Error
			}
			var maxID int
			if err := tx.Model(&model.ImageStorageObject{}).Select("COALESCE(MAX(id),0)").Scan(&maxID).Error; err != nil {
				return err
			}
			return tx.Create(&model.ImageCleanupJob{JobId: "imgclean_" + common.GetUUID(), Scope: "all", Filters: string(data), Source: "scheduled", PolicyID: policy.ID, Status: "queued", CutoffAt: filters.CutoffAt, MaxObjectID: maxID, CreatedAt: now.Unix()}).Error
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type imageCleanupBudget struct {
	mu       sync.Mutex
	next     time.Time
	interval time.Duration
}

// SetRate changes the next request spacing without discarding reservations
// already made by the preceding cleanup pass.
func (budget *imageCleanupBudget) SetRate(perSecond int) {
	budget.mu.Lock()
	budget.interval = time.Second / time.Duration(perSecond)
	budget.mu.Unlock()
}

func (budget *imageCleanupBudget) Wait(ctx context.Context) error {
	budget.mu.Lock()
	now := time.Now()
	if budget.next.Before(now) {
		budget.next = now
	}
	delay := budget.next.Sub(now)
	budget.next = budget.next.Add(budget.interval)
	budget.mu.Unlock()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func RunImageCleanupJob(ctx context.Context, job model.ImageCleanupJob, _ ImageRuntimeConfig) error {
	cfg, err := GetImageCleanupConfig(ctx)
	if err != nil {
		return err
	}
	budget := &imageCleanupBudget{interval: time.Second / time.Duration(cfg.RequestsPerSecond)}
	return runImageCleanupSlice(WithImageStorageRequestBudget(ctx, budget.Wait), job, cfg, budget)
}

func runImageCleanupSlice(parent context.Context, job model.ImageCleanupJob, cfg ImageCleanupConfig, budget *imageCleanupBudget) error {
	dispatchUntil := time.Now().Add(5 * time.Second)
	dispatched := 0
	lease := common.GetUUID()
	claim := model.DB.WithContext(parent).Model(&model.ImageCleanupJob{}).Where("id = ? AND status IN ? AND COALESCE(control, '') = ? AND lease_expires_at <= ?", job.Id, []string{"previewing", "queued", "running"}, "", time.Now().Unix()).Updates(map[string]any{"lease_token": lease, "lease_expires_at": time.Now().Unix() + 120, "control": ""})
	if claim.Error != nil || claim.RowsAffected != 1 {
		return claim.Error
	}
	job.LeaseToken = lease
	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.RequestTimeoutSeconds+8)*time.Second)
	defer cancel()
	heartbeatDone := make(chan struct{})
	defer close(heartbeatDone)
	go func() {
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				renew := model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("id = ? AND lease_token = ? AND lease_expires_at > ?", job.Id, lease, time.Now().Unix()).Update("lease_expires_at", time.Now().Unix()+120)
				if renew.Error != nil || renew.RowsAffected != 1 {
					cancel()
					return
				}
			}
		}
	}()
	// Every I/O has a deadline shorter than the durable lease. The runner's
	// own heartbeat also cancels this context if ownership is lost.
	defer func() {
		finish, stop := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
		defer stop()
		_ = model.DB.WithContext(finish).Model(&model.ImageCleanupJob{}).Where("id = ? AND lease_token = ?", job.Id, lease).Updates(map[string]any{"lease_token": "", "lease_expires_at": 0}).Error
	}()
	if reason := imageCleanupLoad.Check(cfg); reason != "" {
		return model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("id = ? AND lease_token = ?", job.Id, lease).Updates(map[string]any{"last_error": reason, "next_attempt_at": time.Now().Unix() + 15}).Error
	}
	ctx, err := withImageCleanupProfiles(ctx)
	if err != nil {
		return err
	}
	var filters ImageCleanupFilters
	if err := common.UnmarshalJsonStr(job.Filters, &filters); err != nil {
		return failImageCleanup(ctx, job, lease, err)
	}
	filters.IgnoreRetention = imageCleanupIgnoresRetention(job, filters)
	// Old queued jobs have no high-water mark. Freeze one on first execution.
	if job.Source == "" {
		if err := model.DB.WithContext(ctx).Model(&model.ImageStorageObject{}).Select("COALESCE(MAX(id),0)").Scan(&job.MaxObjectID).Error; err != nil {
			return err
		}
		if err := model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("id = ? AND lease_token = ?", job.Id, lease).Updates(map[string]any{"source": "legacy", "max_object_id": job.MaxObjectID}).Error; err != nil {
			return err
		}
	}
	query, err := imageCleanupScope(ctx, job.Scope, filters)
	if err != nil {
		return failImageCleanup(ctx, job, lease, err)
	}
	var objects []model.ImageStorageObject
	if err := query.Where("id > ? AND id <= ?", job.Cursor, job.MaxObjectID).Order("id").Limit(cfg.BatchSize).Find(&objects).Error; err != nil {
		return err
	}
	if job.Status == "previewing" {
		return previewImageCleanupSlice(ctx, job, lease, filters, objects, cfg, dispatchUntil)
	}
	if err := model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("id = ? AND lease_token = ? AND control = ?", job.Id, lease, "").Update("status", "running").Error; err != nil {
		return err
	}
	var retry []model.ImageCleanupRetry
	if err := model.DB.WithContext(ctx).Where("job_id = ? AND outcome = ? AND attempts < ? AND next_attempt_at <= ?", job.JobId, "retry", 3, time.Now().Unix()).Order("id").Limit(min(cfg.BatchSize, 100)).Find(&retry).Error; err != nil {
		return err
	}
	for _, pending := range retry {
		if dispatched >= 100 || !time.Now().Before(dispatchUntil) {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		var object model.ImageStorageObject
		if err := model.DB.WithContext(ctx).Where("object_id = ?", pending.ObjectId).Take(&object).Error; err != nil {
			return err
		}
		dispatched++
		if err := executeImageCleanupObject(ctx, job, lease, object, budget, &pending); err != nil {
			return err
		}
	}
	// A page completes before advancing its cursor. A crash may repeat the
	// page, but object CAS updates make repeated deletes/accounting idempotent.
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, cfg.Concurrency)
	var firstErr error
	var errMu sync.Mutex
	cursor := job.Cursor
	allDispatched := true
	for _, object := range objects {
		if dispatched >= 100 || !time.Now().Before(dispatchUntil) {
			allDispatched = false
			break
		}
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil
		}
		if !time.Now().Before(dispatchUntil) {
			<-semaphore
			allDispatched = false
			break
		}
		latest, loadErr := GetImageCleanupConfig(ctx)
		if loadErr != nil || latest.Concurrency < cfg.Concurrency {
			// A reduction retires this slice's dispatch capacity immediately.
			// In-flight requests drain normally; the next slice uses the new cap.
			<-semaphore
			allDispatched = false
			if loadErr != nil {
				wg.Wait()
				return loadErr
			}
			break
		}
		cursor = object.Id
		dispatched++
		wg.Go(func() {
			defer func() { <-semaphore }()
			defer func() {
				if recover() != nil {
					errMu.Lock()
					firstErr = errors.New("image cleanup worker failed unexpectedly")
					errMu.Unlock()
				}
			}()
			err := executeImageCleanupObject(ctx, job, lease, object, budget, nil)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
			}
		})
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if ctx.Err() != nil {
		return nil
	}
	updates := map[string]any{"cursor": cursor, "status": "queued", "next_attempt_at": time.Now().Unix()}
	if allDispatched && len(objects) < cfg.BatchSize {
		var retries, failures int64
		if err := model.DB.WithContext(ctx).Model(&model.ImageCleanupRetry{}).Where("job_id = ? AND outcome = ? AND attempts < ?", job.JobId, "retry", 3).Count(&retries).Error; err != nil {
			return err
		}
		if retries == 0 {
			if err := model.DB.WithContext(ctx).Model(&model.ImageCleanupRetry{}).Where("job_id = ? AND outcome = ?", job.JobId, "retry").Count(&failures).Error; err != nil {
				return err
			}
			updates["status"], updates["finished_at"] = "completed", time.Now().Unix()
			if failures > 0 || job.Errors > 0 {
				updates["status"] = "completed_with_errors"
			}
		} else {
			updates["next_attempt_at"] = time.Now().Unix() + 15
		}
	}
	return model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("id = ? AND lease_token = ? AND control = ?", job.Id, lease, "").Updates(updates).Error
}

func previewImageCleanupSlice(ctx context.Context, job model.ImageCleanupJob, lease string, filters ImageCleanupFilters, objects []model.ImageStorageObject, cfg ImageCleanupConfig, deadline time.Time) error {
	scan, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	completed := true
	for _, object := range objects {
		if scan.Err() != nil {
			completed = false
			break
		}
		reason, err := imageCleanupProtectionReason(scan, object, filters)
		if err != nil {
			if scan.Err() != nil {
				completed = false
				break
			}
			return err
		}
		if reason == "" {
			if err := EnsureImageObjectExclusive(scan, object); err != nil {
				if scan.Err() != nil {
					completed = false
					break
				}
				if errors.Is(err, errImageSharedLocation) || errors.Is(err, gorm.ErrRecordNotFound) {
					reason = "location"
				} else {
					return err
				}
			}
		}
		job.Scanned++
		if reason == "" {
			job.Matched++
			job.MatchedBytes += object.ByteSize
		} else {
			job.Protected++
			switch reason {
			case "reference":
				job.ReferenceSkipped++
			case "retention":
				job.RetentionSkipped++
			case "location":
				job.LocationSkipped++
			}
		}
		job.Cursor = object.Id
	}
	updates := map[string]any{"cursor": job.Cursor, "scanned": job.Scanned, "matched": job.Matched, "matched_bytes": job.MatchedBytes, "protected": job.Protected, "reference_skipped": job.ReferenceSkipped, "retention_skipped": job.RetentionSkipped, "location_skipped": job.LocationSkipped, "last_error": ""}
	updates["next_attempt_at"] = time.Now().Unix()
	if !completed {
		updates["last_error"] = "Preview scanning reached its resource budget and will continue"
	}
	if completed && len(objects) < cfg.BatchSize {
		token, err := SignImageCleanupPreview(job.Scope, filters, job.UserId, time.Now().Unix())
		if err != nil {
			return failImageCleanup(ctx, job, lease, err)
		}
		updates["status"], updates["preview_token"], updates["finished_at"] = "preview_ready", token, time.Now().Unix()
	}
	return model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("id = ? AND lease_token = ? AND status = ?", job.Id, lease, "previewing").Updates(updates).Error
}

func failImageCleanup(ctx context.Context, job model.ImageCleanupJob, lease string, cause error) error {
	return model.DB.WithContext(ctx).Model(&model.ImageCleanupJob{}).Where("id = ? AND lease_token = ?", job.Id, lease).Updates(map[string]any{"status": "failed", "last_error": fmt.Sprintf("Cleanup configuration is unavailable: %s", SanitizeAsyncImageDiagnostic(cause.Error(), 200)), "finished_at": time.Now().Unix()}).Error
}
