package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

var durationBuckets = []float64{0.1, 0.5, 1, 5, 30, 90}

type workspaceMetrics struct {
	submitted       uint64
	completed       uint64
	failed          uint64
	cancelled       uint64
	retried         uint64
	durationN       uint64
	durationSum     float64
	durationBuckets [7]uint64
}

type Metrics struct {
	mu         sync.Mutex
	workspaces map[string]*workspaceMetrics
}

func NewMetrics() *Metrics { return &Metrics{workspaces: make(map[string]*workspaceMetrics)} }

func (m *Metrics) workspace(id string) *workspaceMetrics {
	counts := m.workspaces[id]
	if counts == nil {
		counts = &workspaceMetrics{}
		m.workspaces[id] = counts
	}
	return counts
}

func (m *Metrics) TaskSubmitted(workspaceID string) {
	m.mu.Lock()
	m.workspace(workspaceID).submitted++
	m.mu.Unlock()
}

func (m *Metrics) TaskCancelled(workspaceID string) {
	m.mu.Lock()
	m.workspace(workspaceID).cancelled++
	m.mu.Unlock()
}

func (m *Metrics) TaskRetried(workspaceID string) {
	m.mu.Lock()
	m.workspace(workspaceID).retried++
	m.mu.Unlock()
}

func (m *Metrics) RecordTask(workspaceID, status string, duration time.Duration, retried bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	counts := m.workspace(workspaceID)
	switch status {
	case "completed":
		counts.completed++
	case "failed":
		counts.failed++
	case "cancelled":
		counts.cancelled++
	}
	if retried {
		counts.retried++
	}
	seconds := duration.Seconds()
	counts.durationN++
	counts.durationSum += seconds
	for i, bound := range durationBuckets {
		if seconds <= bound {
			counts.durationBuckets[i]++
			break
		}
	}
	if seconds > durationBuckets[len(durationBuckets)-1] {
		counts.durationBuckets[len(counts.durationBuckets)-1]++
	}
}

func (m *Metrics) Handler(store TaskStore) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := principalFromContext(r.Context())
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		queue, err := store.QueueStats(ctx, principal.WorkspaceID)
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, "metrics temporarily unavailable")
			return
		}
		m.mu.Lock()
		counts := *m.workspace(principal.WorkspaceID)
		m.mu.Unlock()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintln(w, "# HELP modelrail_tasks_submitted_total Tasks accepted in this workspace by this process.")
		fmt.Fprintln(w, "# TYPE modelrail_tasks_submitted_total counter")
		fmt.Fprintf(w, "modelrail_tasks_submitted_total %d\n", counts.submitted)
		fmt.Fprintln(w, "# HELP modelrail_tasks_completed_total Tasks completed in this workspace by this process.")
		fmt.Fprintln(w, "# TYPE modelrail_tasks_completed_total counter")
		fmt.Fprintf(w, "modelrail_tasks_completed_total %d\n", counts.completed)
		fmt.Fprintln(w, "# HELP modelrail_tasks_failed_total Tasks terminally failed in this workspace by this process.")
		fmt.Fprintln(w, "# TYPE modelrail_tasks_failed_total counter")
		fmt.Fprintf(w, "modelrail_tasks_failed_total %d\n", counts.failed)
		fmt.Fprintln(w, "# HELP modelrail_tasks_cancelled_total Tasks cancelled in this workspace by this process.")
		fmt.Fprintln(w, "# TYPE modelrail_tasks_cancelled_total counter")
		fmt.Fprintf(w, "modelrail_tasks_cancelled_total %d\n", counts.cancelled)
		fmt.Fprintln(w, "# HELP modelrail_tasks_retried_total Attempts requeued in this workspace.")
		fmt.Fprintln(w, "# TYPE modelrail_tasks_retried_total counter")
		fmt.Fprintf(w, "modelrail_tasks_retried_total %d\n", counts.retried)
		fmt.Fprintln(w, "# HELP modelrail_task_duration_seconds Processing duration in seconds.")
		fmt.Fprintln(w, "# TYPE modelrail_task_duration_seconds histogram")
		cumulative := uint64(0)
		for i, bound := range durationBuckets {
			cumulative += counts.durationBuckets[i]
			fmt.Fprintf(w, "modelrail_task_duration_seconds_bucket{le=\"%g\"} %d\n", bound, cumulative)
		}
		cumulative += counts.durationBuckets[len(counts.durationBuckets)-1]
		fmt.Fprintf(w, "modelrail_task_duration_seconds_bucket{le=\"+Inf\"} %d\n", cumulative)
		fmt.Fprintf(w, "modelrail_task_duration_seconds_sum %g\n", counts.durationSum)
		fmt.Fprintf(w, "modelrail_task_duration_seconds_count %d\n", counts.durationN)

		fmt.Fprintln(w, "# HELP modelrail_queue_tasks Current task count by status in this workspace.")
		fmt.Fprintln(w, "# TYPE modelrail_queue_tasks gauge")
		statuses := make([]string, 0, len(queue))
		for status := range queue {
			statuses = append(statuses, status)
		}
		sort.Strings(statuses)
		for _, status := range statuses {
			fmt.Fprintf(w, "modelrail_queue_tasks{status=\"%s\"} %d\n", status, queue[status])
		}
	})
}
