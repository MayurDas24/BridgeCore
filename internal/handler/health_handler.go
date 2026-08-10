package handler

import (
	"net/http"
	"time"

	"github.com/bridgecore/bridgecore/internal/database"
	"github.com/bridgecore/bridgecore/pkg/response"
)

// HealthHandler exposes /health, /ready, and /live for orchestrator probes
// (Kubernetes liveness/readiness, load balancer health checks, uptime
// monitors).
type HealthHandler struct {
	db        *database.DB
	redis     *database.Redis
	version   string
	buildTime string
	startedAt time.Time
}

func NewHealthHandler(db *database.DB, redis *database.Redis, version, buildTime string) *HealthHandler {
	return &HealthHandler{db: db, redis: redis, version: version, buildTime: buildTime, startedAt: time.Now()}
}

// Health godoc
// @Summary      Full health report (app, database, redis, version, uptime)
// @Tags         health
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /health [get]
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	dbOK := h.db.Healthy(r.Context())
	redisOK := h.redis.Healthy(r.Context())

	status := "healthy"
	httpStatus := http.StatusOK
	if !dbOK || !redisOK {
		status = "degraded"
		httpStatus = http.StatusServiceUnavailable
	}

	payload := map[string]any{
		"status": status,
		"database": map[string]any{
			"status": boolToStatus(dbOK),
		},
		"redis": map[string]any{
			"status": boolToStatus(redisOK),
		},
		"version":    h.version,
		"build_time": h.buildTime,
		"uptime":     time.Since(h.startedAt).String(),
	}

	if httpStatus == http.StatusOK {
		response.OK(w, "service is healthy", payload)
	} else {
		response.Err(w, httpStatus, "service is degraded", payload)
	}
}

// Ready godoc
// @Summary      Readiness probe — checks downstream dependencies
// @Tags         health
// @Produce      json
// @Success      200 {object} response.Envelope
// @Failure      503 {object} response.Envelope
// @Router       /ready [get]
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	dbOK := h.db.Healthy(r.Context())
	redisOK := h.redis.Healthy(r.Context())

	if dbOK && redisOK {
		response.OK(w, "ready", map[string]any{"database": "up", "redis": "up"})
		return
	}
	response.Err(w, http.StatusServiceUnavailable, "not ready", map[string]any{
		"database": boolToStatus(dbOK),
		"redis":    boolToStatus(redisOK),
	})
}

// Live godoc
// @Summary      Liveness probe — process is up and serving
// @Tags         health
// @Produce      json
// @Success      200 {object} response.Envelope
// @Router       /live [get]
func (h *HealthHandler) Live(w http.ResponseWriter, r *http.Request) {
	response.OK(w, "alive", map[string]any{"uptime": time.Since(h.startedAt).String()})
}

func boolToStatus(ok bool) string {
	if ok {
		return "up"
	}
	return "down"
}
