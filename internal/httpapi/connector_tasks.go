package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"github.com/gin-gonic/gin"
)

type connectorTasksHandlers struct{ *deps }

func decodeConnectorTask(c *gin.Context, destination any, limit int64) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, limit))
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid typed task payload"})
		return false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid typed task payload"})
		return false
	}
	return true
}
func (h *connectorTasksHandlers) owner(c *gin.Context) (int64, bool) {
	user, ok := h.requestUser(c.Request, true)
	if !ok || user.ID <= 0 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return 0, false
	}
	c.Header("Cache-Control", "no-store")
	return user.ID, true
}
func connectorTaskError(c *gin.Context, err error) {
	status, message := http.StatusInternalServerError, "connector task operation failed"
	switch {
	case errors.Is(err, repo.ErrConnectorCredential):
		status, message = http.StatusUnauthorized, "invalid Agent credential"
	case errors.Is(err, repo.ErrConnectorTaskNotFound):
		status, message = http.StatusNotFound, "connector task or target Agent not found"
	case errors.Is(err, repo.ErrConnectorTaskConflict):
		status, message = http.StatusConflict, "idempotency key already has different task input"
	case errors.Is(err, repo.ErrConnectorTaskLease):
		status, message = http.StatusConflict, "task lease or attempt is no longer valid"
	case errors.Is(err, repo.ErrConnectorAgentBusy):
		status, message = http.StatusConflict, "Agent already holds a task or needs execution recovery"
	}
	c.JSON(status, gin.H{"error": message})
}
func (h *connectorTasksHandlers) collection(c *gin.Context) {
	owner, ok := h.owner(c)
	if !ok {
		return
	}
	if c.Request.Method == http.MethodGet {
		tasks, err := h.repo.ConnectorTaskList(owner, time.Now().UTC())
		if err != nil {
			connectorTaskError(c, err)
			return
		}
		c.JSON(http.StatusOK, tasks)
		return
	}
	var input domain.ConnectorTaskRequest
	if !decodeConnectorTask(c, &input, 2<<20) {
		return
	}
	normalized, err := domain.NormalizeConnectorTaskRequest(input)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid typed connector task"})
		return
	}
	task, created, err := h.repo.SubmitConnectorTask(owner, "ctask-"+randomID(), normalized, time.Now().UTC())
	if err != nil {
		connectorTaskError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, task)
}
func (h *connectorTasksHandlers) detail(c *gin.Context) {
	owner, ok := h.owner(c)
	if !ok {
		return
	}
	task, err := h.repo.ConnectorTask(owner, c.Param("id"), time.Now().UTC())
	if err != nil {
		connectorTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}
func (h *connectorTasksHandlers) cancel(c *gin.Context) {
	owner, ok := h.owner(c)
	if !ok {
		return
	}
	task, err := h.repo.CancelConnectorTask(owner, c.Param("id"), time.Now().UTC())
	if err != nil {
		connectorTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}
func (h *connectorTasksHandlers) connectorTaskAgentHash(c *gin.Context) (string, bool) {
	raw, ok := bearerToken(c.Request)
	if !ok || !domain.ValidConnectorToken(raw, domain.ConnectorAgentTokenPrefix) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid Agent credential"})
		return "", false
	}
	hash := domain.ConnectorTokenHash(raw)
	if err := h.repo.ValidateConnectorTaskCredential(hash, time.Now().UTC()); err != nil {
		connectorTaskError(c, err)
		return "", false
	}
	c.Header("Cache-Control", "no-store")
	return hash, true
}
func (h *connectorTasksHandlers) claim(c *gin.Context) {
	hash, ok := h.connectorTaskAgentHash(c)
	if !ok {
		return
	}
	var input struct {
		LeaseToken string `json:"leaseToken"`
	}
	if !decodeConnectorTask(c, &input, 4096) {
		return
	}
	if !domain.ValidConnectorToken(input.LeaseToken, domain.ConnectorLeaseTokenPrefix) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid lease proof"})
		return
	}
	task, err := h.repo.ClaimConnectorTask(hash, input.LeaseToken, time.Now().UTC())
	if err != nil {
		connectorTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"task": task})
}
func (h *connectorTasksHandlers) action(c *gin.Context) {
	hash, ok := h.connectorTaskAgentHash(c)
	if !ok {
		return
	}
	action := c.Param("action")
	var proof domain.ConnectorTaskLease
	var result *domain.ConnectorTaskResult
	if action == "complete" {
		var input domain.ConnectorTaskCompletion
		if !decodeConnectorTask(c, &input, 8<<20) {
			return
		}
		normalized, err := domain.NormalizeConnectorTaskResult(input.Result)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "result requires bounded output and confirmed process quiescence"})
			return
		}
		proof = domain.ConnectorTaskLease{Attempt: input.Attempt, LeaseToken: input.LeaseToken}
		result = &normalized
	} else {
		switch action {
		case "start", "renew", "control":
		default:
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown task action"})
			return
		}
		if !decodeConnectorTask(c, &proof, 4096) {
			return
		}
	}
	if proof.Attempt < 1 || !domain.ValidConnectorToken(proof.LeaseToken, domain.ConnectorLeaseTokenPrefix) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid lease proof"})
		return
	}
	task, err := h.repo.ConnectorTaskAction(hash, c.Param("id"), action, proof, result, time.Now().UTC())
	if err != nil {
		connectorTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, task)
}

func (h *connectorTasksHandlers) events(c *gin.Context) {
	owner, ok := h.owner(c)
	if !ok {
		return
	}
	after := int64(0)
	if raw := c.Query("after"); raw != "" {
		var err error
		after, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || after < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid event cursor"})
			return
		}
	}
	events, err := h.repo.ConnectorTaskEvents(owner, c.Param("id"), after)
	if err != nil {
		connectorTaskError(c, err)
		return
	}
	c.JSON(http.StatusOK, events)
}
