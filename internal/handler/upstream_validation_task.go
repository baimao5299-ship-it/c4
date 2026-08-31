// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

const upstreamValidationTaskTTL = 15 * time.Minute

type upstreamValidationTaskStatus string

const (
	upstreamValidationTaskQueued    upstreamValidationTaskStatus = "queued"
	upstreamValidationTaskRunning   upstreamValidationTaskStatus = "running"
	upstreamValidationTaskCompleted upstreamValidationTaskStatus = "completed"
	upstreamValidationTaskFailed    upstreamValidationTaskStatus = "failed"
)

// upstreamValidationTask is intentionally process-local. The service's
// advisory lock and the database snapshot remain the source of truth across
// instances; this object only gives the browser a short-lived progress view.
type upstreamValidationTask struct {
	mu        sync.RWMutex
	id        string
	createdAt time.Time
	status    upstreamValidationTaskStatus
	progress  service.UpstreamValidationProgress
	result    *UpstreamValidationSummary
	errorText string
}

type upstreamValidationTaskResponse struct {
	TaskID           string                       `json:"task_id"`
	Status           upstreamValidationTaskStatus `json:"status"`
	UpstreamsTotal   int                          `json:"upstreams_total"`
	UpstreamsChecked int                          `json:"upstreams_checked"`
	ModelsTotal      int                          `json:"models_total"`
	ModelsChecked    int                          `json:"models_checked"`
	ModelsAvailable  int                          `json:"models_available"`
	ModelsFailed     int                          `json:"models_failed"`
	Result           *UpstreamValidationSummary   `json:"result,omitempty"`
	Error            string                       `json:"error,omitempty"`
}

func newUpstreamValidationTask() *upstreamValidationTask {
	return &upstreamValidationTask{
		id:        uuid.NewString(),
		createdAt: time.Now(),
		status:    upstreamValidationTaskQueued,
	}
}

func (t *upstreamValidationTask) setRunning() {
	t.mu.Lock()
	t.status = upstreamValidationTaskRunning
	t.mu.Unlock()
}

func (t *upstreamValidationTask) updateProgress(progress service.UpstreamValidationProgress) {
	t.mu.Lock()
	// Ignore a late progress callback after the worker has settled. This keeps
	// a failed task from moving back to a running-looking state.
	if t.status == upstreamValidationTaskQueued || t.status == upstreamValidationTaskRunning {
		t.progress = progress
	}
	t.mu.Unlock()
}

func (t *upstreamValidationTask) finish(result *service.UpstreamValidationSummary, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.status = upstreamValidationTaskFailed
		t.errorText = strings.TrimSpace(err.Error())
		if t.errorText == "" {
			t.errorText = "upstream validation failed"
		}
		return
	}
	t.status = upstreamValidationTaskCompleted
	converted := toAPIUpstreamValidationSummary(result)
	t.result = &converted
	t.progress.Done = true
}

func (t *upstreamValidationTask) snapshot() upstreamValidationTaskResponse {
	t.mu.RLock()
	defer t.mu.RUnlock()
	response := upstreamValidationTaskResponse{
		TaskID:           t.id,
		Status:           t.status,
		UpstreamsTotal:   t.progress.UpstreamsTotal,
		UpstreamsChecked: t.progress.UpstreamsChecked,
		ModelsTotal:      t.progress.ModelsTotal,
		ModelsChecked:    t.progress.ModelsChecked,
		ModelsAvailable:  t.progress.ModelsAvailable,
		ModelsFailed:     t.progress.ModelsFailed,
		Error:            t.errorText,
	}
	if t.result != nil {
		copy := *t.result
		response.Result = &copy
	}
	return response
}

func (h *AdminAPI) cleanupValidationTasks(now time.Time) {
	h.validationTasksMu.Lock()
	defer h.validationTasksMu.Unlock()
	if h.validationTasks == nil {
		h.validationTasks = make(map[string]*upstreamValidationTask)
		return
	}
	for id, task := range h.validationTasks {
		task.mu.RLock()
		active := task.status == upstreamValidationTaskQueued || task.status == upstreamValidationTaskRunning
		createdAt := task.createdAt
		task.mu.RUnlock()
		if !active && now.Sub(createdAt) > upstreamValidationTaskTTL {
			delete(h.validationTasks, id)
		}
	}
}

// PostUpstreamsValidateAllStart starts validation without holding the browser
// request open. The legacy synchronous endpoint remains available for older
// API clients.
func (h *AdminAPI) PostUpstreamsValidateAllStart(w http.ResponseWriter, r *http.Request) {
	if h == nil {
		httpface.WriteErr(w, http.StatusInternalServerError, "validation service is not configured")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	h.cleanupValidationTasks(time.Now())
	task := newUpstreamValidationTask()
	h.validationTasksMu.Lock()
	if h.validationTasks == nil {
		h.validationTasks = make(map[string]*upstreamValidationTask)
	}
	h.validationTasks[task.id] = task
	h.validationTasksMu.Unlock()

	go func() {
		task.setRunning()
		result, err := h.svc.ValidateAllUpstreamsWithProgress(context.Background(), task.updateProgress)
		task.finish(result, err)
	}()

	httpface.WriteJSON(w, http.StatusAccepted, map[string]any{
		"task_id": task.id,
		"status":  upstreamValidationTaskQueued,
	})
}

// GetUpstreamsValidateAllTask returns a point-in-time progress snapshot. A
// missing or expired task is a normal 404 so the UI can ask the operator to
// start a fresh validation rather than displaying stale numbers.
func (h *AdminAPI) GetUpstreamsValidateAllTask(w http.ResponseWriter, r *http.Request, taskID string) {
	if h == nil {
		httpface.WriteErr(w, http.StatusInternalServerError, "validation service is not configured")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	id := strings.TrimSpace(taskID)
	if id == "" {
		httpface.WriteErr(w, http.StatusBadRequest, "task_id is required")
		return
	}
	h.validationTasksMu.RLock()
	task := h.validationTasks[id]
	h.validationTasksMu.RUnlock()
	if task == nil {
		httpface.WriteErr(w, http.StatusNotFound, "validation task not found")
		return
	}
	httpface.WriteJSON(w, http.StatusOK, task.snapshot())
}
