package platform

import (
	"path/filepath"
	"runtime"
)

// Where a provisioned AI environment lives.
//
// The layout is defined here and nowhere else, because four things have to
// agree on it: the resolver that finds the interpreter, the provisioner that
// creates it, the doctor that reports on it, and the interface that shows
// where the disk went. A second copy of these joins is how a provisioned
// environment comes to exist in a place nothing looks.
//
// Everything is under the data directory on purpose. Provisioning writes
// several hundred megabytes of interpreter, wheels and cache; putting all of it
// in one directory means a user who wants the space back deletes one directory,
// and nothing outside the application's own data directory is ever touched.

// RuntimeDir is where a provisioned AI environment lives.
func RuntimeDir(dataDir string) string {
	return filepath.Join(dataDir, "runtime")
}

// RuntimePythonDir is where uv installs the interpreters it manages. Passed as
// UV_PYTHON_INSTALL_DIR.
func RuntimePythonDir(runtimeDir string) string {
	return filepath.Join(runtimeDir, "python")
}

// RuntimeVenvDir is the virtual environment the worker runs from. Passed as
// UV_PROJECT_ENVIRONMENT.
func RuntimeVenvDir(runtimeDir string) string {
	return filepath.Join(runtimeDir, "venv")
}

// RuntimeCacheDir is uv's download cache for this installation. Passed as
// UV_CACHE_DIR.
//
// Redirected rather than left at uv's default so that a user's home directory
// is never written to, and so that a failed or abandoned install leaves nothing
// behind outside the data directory.
func RuntimeCacheDir(runtimeDir string) string {
	return filepath.Join(runtimeDir, "cache")
}

// RuntimeVenvPython is the interpreter inside the provisioned environment.
//
// This, and not the interpreter uv installs, is what the worker runs from: the
// venv is what has the dependencies in it. uv's install directory has a
// version-stamped layout that is not worth depending on.
func RuntimeVenvPython(runtimeDir string) string {
	venv := RuntimeVenvDir(runtimeDir)
	if runtime.GOOS == "windows" {
		return filepath.Join(venv, "Scripts", "python.exe")
	}
	return filepath.Join(venv, "bin", "python")
}

// RuntimeManifestPath is the record of what was provisioned.
func RuntimeManifestPath(runtimeDir string) string {
	return filepath.Join(runtimeDir, "provision.json")
}
