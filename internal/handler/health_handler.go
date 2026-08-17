package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// QueueDepthReporter reports how many export jobs are waiting, so a backlog
// is visible to CloudWatch without a second scraping mechanism.
type QueueDepthReporter interface {
	QueueDepth(ctx context.Context) (int64, error)
	Backend() string
}

// HealthHandler exposes /live, /ready and /health.
//
// The three are genuinely different, and conflating them is a common way to
// build an unrecoverable deployment:
//
//   - /live answers "is this process running". It must never check a
//     dependency, because if a liveness probe fails when the database is
//     briefly unreachable, the orchestrator kills every task at once and
//     turns a recoverable database blip into a total outage.
//   - /ready answers "should this task receive traffic". It does check
//     dependencies, so a task with a broken database is quietly removed from
//     the load balancer and put back when it recovers.
//   - /health is the human/diagnostic view: everything, with detail.
type HealthHandler struct {
	db        *database.DB
	redis     *database.Redis
	exports   QueueDepthReporter
	version   string
	buildTime string
	gitCommit string
	env       string
	startedAt time.Time
}

func NewHealthHandler(
	db *database.DB,
	redis *database.Redis,
	exportQueue QueueDepthReporter,
	version, buildTime, gitCommit, env string,
) *HealthHandler {
	return &HealthHandler{
		db:        db,
		redis:     redis,
		exports:   exportQueue,
		version:   version,
		buildTime: buildTime,
		gitCommit: gitCommit,
		env:       env,
		startedAt: time.Now(),
	}
}

// Health godoc
// @Summary      Full health report (dependencies, version, uptime, queue depth)
// @Tags         health
// @Produce      json
// @Success      200 {object} response.Envelope
// @Failure      503 {object} response.Envelope
// @Router       /health [get]
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	dbOK := h.db.Healthy(r.Context())
	redisOK := h.redis.Healthy(r.Context())

	payload := map[string]any{
		"status":     statusWord(dbOK && redisOK),
		"database":   boolToStatus(dbOK),
		"redis":      boolToStatus(redisOK),
		"env":        h.env,
		"version":    h.version,
		"build_time": h.buildTime,
		"git_commit": h.gitCommit,
		"uptime":     time.Since(h.startedAt).Round(time.Second).String(),
		"uptime_s":   int64(time.Since(h.startedAt).Seconds()),
	}

	if h.exports != nil {
		exportInfo := map[string]any{"object_store": h.exports.Backend()}
		// A failed queue-depth read is reported, not fatal: the export
		// pipeline being unobservable is not the same as the API being
		// unhealthy.
		if depth, err := h.exports.QueueDepth(r.Context()); err == nil {
			exportInfo["queue_depth"] = depth
		} else {
			exportInfo["queue_depth"] = "unavailable"
		}
		payload["exports"] = exportInfo
	}

	if dbOK && redisOK {
		response.OKWithRequest(w, r, "service is healthy", payload)
		return
	}
	response.Err(w, http.StatusServiceUnavailable, "service is degraded", payload)
}

// Ready godoc
// @Summary      Readiness probe — checks downstream dependencies
// @Description  Used by the ALB target group. A failing task is removed from rotation without being restarted.
// @Tags         health
// @Produce      json
// @Success      200 {object} response.Envelope
// @Failure      503 {object} response.Envelope
// @Router       /ready [get]
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	dbOK := h.db.Healthy(ctx)
	redisOK := h.redis.Healthy(ctx)

	if dbOK && redisOK {
		response.OKWithRequest(w, r, "ready", map[string]any{"database": "up", "redis": "up"})
		return
	}
	response.Err(w, http.StatusServiceUnavailable, "not ready", map[string]any{
		"database": boolToStatus(dbOK),
		"redis":    boolToStatus(redisOK),
	})
}

// Live godoc
// @Summary      Liveness probe — the process is up and serving
// @Description  Deliberately dependency-free: a database outage must not cause every task to be restarted.
// @Tags         health
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /live [get]
func (h *HealthHandler) Live(w http.ResponseWriter, r *http.Request) {
	response.OKWithRequest(w, r, "alive", map[string]any{
		"uptime":  time.Since(h.startedAt).Round(time.Second).String(),
		"version": h.version,
	})
}

func boolToStatus(ok bool) string {
	if ok {
		return "up"
	}
	return "down"
}

func statusWord(ok bool) string {
	if ok {
		return "healthy"
	}
	return "degraded"
}
