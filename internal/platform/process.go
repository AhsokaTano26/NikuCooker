package platform

import (
	"fmt"
	"os/exec"
	"runtime"
)

// KillProcessTree terminates a process and anything it spawned.
//
// On Windows a plain Kill leaves children behind, because the process model
// there does not propagate termination. taskkill /T is the only reliable
// process-tree kill.
//
// It lives in this package because two callers need it and they have nothing
// else in common: the worker pool, which kills a Python process that has gone
// quiet, and provisioning, which kills uv mid-download when the user cancels.
func KillProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}

	if runtime.GOOS == "windows" {
		// The pid comes from os/exec, never from user input, so there is no
		// interpolation concern; the arguments are still passed as a vector.
		kill := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(cmd.Process.Pid))
		_ = kill.Run()
		return
	}

	_ = cmd.Process.Kill()
}
