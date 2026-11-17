package httpapi

import (
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// Portable connector downloads never trigger a build, installer or network
// access on the NAS. A release must ship each advertised prebuilt artifact.
func serveConnectorBinary(c *gin.Context) {
	targetOS, targetArch := c.Query("os"), c.Query("arch")
	switch targetOS {
	case "linux", "darwin", "windows":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "choose linux, darwin or windows"})
		return
	}
	if targetArch != "amd64" && targetArch != "arm64" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "choose amd64 or arm64"})
		return
	}
	pattern := strings.TrimSpace(os.Getenv("AGENTFLOW_CONNECTOR_BINARY"))
	if pattern == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "portable connector artifacts are not deployed"})
		return
	}
	ext := ""
	if targetOS == "windows" {
		ext = ".exe"
	}
	path := strings.NewReplacer("{os}", targetOS, "{arch}", targetArch, "{ext}", ext).Replace(pattern)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "requested portable connector artifact is not deployed"})
		return
	}
	serveBinaryArtifact(c, path, "loopforge-connect"+ext)
}
