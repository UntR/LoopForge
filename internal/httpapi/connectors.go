package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"github.com/gin-gonic/gin"
)

type connectorsHandlers struct{ *deps }

func newConnectorSecret(prefix string) (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func (h *connectorsHandlers) issueCode(c *gin.Context) {
	user, ok := h.requestUser(c.Request, true)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	code, err := newConnectorSecret(domain.ConnectorBindCodePrefix)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot issue bind code"})
		return
	}
	expires := time.Now().UTC().Add(10 * time.Minute)
	if err := h.repo.IssueConnectorCode(user.ID, domain.ConnectorTokenHash(code), expires); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot issue bind code"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, gin.H{"code": code, "expiresAt": expires})
}

func (h *connectorsHandlers) enroll(c *gin.Context) {
	var input domain.ConnectorEnrollment
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid connector enrollment"})
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || domain.ValidateConnectorEnrollment(input) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid connector enrollment"})
		return
	}
	agentIDs := make([]string, len(input.Agents))
	for i := range agentIDs {
		agentIDs[i] = "agent-" + randomID()
	}
	identity, err := h.repo.EnrollConnector(input, "device-"+randomID(), agentIDs, time.Now().UTC())
	if errors.Is(err, repo.ErrConnectorCredential) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "bind code is invalid, used or expired; recover pending enrollment or request a fresh code"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot enroll connector"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusCreated, identity)
}

func (h *connectorsHandlers) deviceSelf(c *gin.Context) {
	token, ok := bearerToken(c.Request)
	if !ok || !domain.ValidConnectorToken(token, domain.ConnectorDeviceTokenPrefix) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid device credential"})
		return
	}
	identity, err := h.repo.ConnectorIdentityByHash(domain.ConnectorTokenHash(token), time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid device credential"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, identity)
}

func (h *connectorsHandlers) agentSelf(c *gin.Context) {
	token, ok := bearerToken(c.Request)
	if !ok || !domain.ValidConnectorToken(token, domain.ConnectorAgentTokenPrefix) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid Agent credential"})
		return
	}
	agent, err := h.repo.ConnectorAgentByHash(domain.ConnectorTokenHash(token), time.Now().UTC())
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid Agent credential"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, agent)
}

func (h *connectorsHandlers) list(c *gin.Context) {
	user, ok := h.requestUser(c.Request, true)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	identities, err := h.repo.ListConnectors(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot list connectors"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, identities)
}

func (h *connectorsHandlers) revoke(c *gin.Context) {
	user, ok := h.requestUser(c.Request, true)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	var err error
	if c.Param("kind") == "devices" {
		err = h.repo.RevokeConnectorDevice(user.ID, c.Param("id"))
	} else if c.Param("kind") == "agents" {
		err = h.repo.RevokeConnectorAgent(user.ID, c.Param("id"))
	} else {
		c.JSON(http.StatusNotFound, gin.H{"error": "connector not found"})
		return
	}
	if errors.Is(err, repo.ErrConnectorNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"error": "connector not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot revoke connector"})
		return
	}
	c.Status(http.StatusNoContent)
}
