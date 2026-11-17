package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestConnectorBinaryPrebuiltWindowsAndStrictPlatform(t *testing.T) {
	root := t.TempDir()
	data := []byte("synthetic connector artifact")
	if err := os.WriteFile(filepath.Join(root, "connect-windows-amd64.exe"), data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_CONNECTOR_BINARY", filepath.Join(root, "connect-{os}-{arch}{ext}"))
	h := gin.New()
	h.GET("/runners/loopforge-connect", serveConnectorBinary)
	for _, tc := range []struct {
		query  string
		status int
	}{
		{"?os=windows&arch=amd64", 200}, {"?os=windows&arch=amd64&metadata=1", 200},
		{"?os=linux&arch=arm64", 404}, {"?os=windows&arch=..", 400}, {"?os=windows/..&arch=amd64", 400}, {"?os=&arch=amd64", 400},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/runners/loopforge-connect"+tc.query, nil))
		if rec.Code != tc.status {
			t.Fatalf("%s => %d", tc.query, rec.Code)
		}
		if tc.query == "?os=windows&arch=amd64" && rec.Body.String() != string(data) {
			t.Fatal("wrong artifact")
		}
		if tc.query == "?os=windows&arch=amd64&metadata=1" {
			var metadata struct {
				SHA256 string `json:"sha256"`
			}
			json.Unmarshal(rec.Body.Bytes(), &metadata)
			sum := sha256.Sum256(data)
			if metadata.SHA256 != hex.EncodeToString(sum[:]) {
				t.Fatal("wrong digest")
			}
		}
	}
}
