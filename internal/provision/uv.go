package provision

import (
	"path/filepath"
	"runtime"

	"github.com/AhsokaTano26/NikuCooker/internal/platform"
)

// FindUV locates the uv to provision with.
//
// Beside the executable first, because that is where a release archive puts it
// and it is the copy whose version this binary was tested against. PATH second,
// which is what makes provisioning work from a source checkout and on a machine
// where the user already has uv.
//
// lookPath is injected so the search can be tested without a filesystem.
func FindUV(exeDir string, lookPath func(string) (string, error)) (string, error) {
	name := "uv"
	if runtime.GOOS == "windows" {
		name = "uv.exe"
	}

	if exeDir != "" {
		bundled := filepath.Join(exeDir, name)
		if platform.Exists(bundled) {
			return bundled, nil
		}
	}

	if path, err := lookPath("uv"); err == nil && path != "" {
		return path, nil
	}

	return "", ErrNoUV
}
