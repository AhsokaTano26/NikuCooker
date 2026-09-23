package models

import (
	"path/filepath"
	"testing"

	"github.com/AhsokaTano26/NikuCooker/internal/database"
)

// Removing runtime source switching, or only changing the configuration while
// leaving the downloader on its startup URL, must fail this test.
func TestServiceSwitchesDownloadEndpointAtRuntime(t *testing.T) {
	db, err := database.Open(database.Options{Path: filepath.Join(t.TempDir(), "models.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	service, err := New(Options{DB: db, ModelDir: filepath.Join(t.TempDir(), "models")})
	if err != nil {
		t.Fatal(err)
	}

	dynamic, ok := any(service).(interface {
		SetEndpoint(string)
		Endpoint() string
	})
	if !ok {
		t.Fatal("model service does not support changing the endpoint at runtime")
	}

	dynamic.SetEndpoint("https://mirror.example.com/")
	if got := dynamic.Endpoint(); got != "https://mirror.example.com" {
		t.Errorf("Endpoint() = %q, want the selected mirror without a trailing slash", got)
	}
}
