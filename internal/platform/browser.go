package platform

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

// ErrNoOpener reports that this system has no program for opening a URL.
//
// A distinct error because it is not a failure. A container, a build machine
// and a server reached over SSH all lack one, and none of them is broken.
// Callers use it to stay quiet about a condition that has no fix and no
// consequence.
var ErrNoOpener = errors.New("platform: no program for opening a URL is installed")

// OpenBrowser opens a URL in the user's default browser.
//
// The process is started and not waited on. `xdg-open` in particular stays in
// the foreground until the browser it launched exits, so a server that waited
// would hold a child for as long as the user kept a tab open — which, for a
// program meant to run for weeks, is the rest of its life.
func OpenBrowser(ctx context.Context, url string) error {
	name, args := opener(url)

	// Resolved before spawning, so that "there is no browser here" is a value
	// the caller can recognise rather than one exec error among many.
	path, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("%w (looked for %s)", ErrNoOpener, name)
	}

	// Stdout and Stderr are left nil, which connects them to the null device: a
	// missing display, an unset $BROWSER and a browser that prints warnings are
	// all things the user can do nothing about from here, and the server's log
	// is not the place to relay them.
	cmd := exec.CommandContext(ctx, path, args...)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("platform: open %s: %w", url, err)
	}

	// Reaped in the background. Not waiting at all would leave a zombie for as
	// long as this process lives, and waiting here would block the caller on
	// the browser's lifetime.
	go func() { _ = cmd.Wait() }()

	return nil
}

// opener names the program and arguments that open a URL on this system.
//
// Every argument is passed separately rather than as a shell string. A URL is
// not a constant: it carries a host and a port chosen by whoever started the
// server, and handing that to a shell would let the server execute whatever the
// string turned out to contain.
func opener(url string) (string, []string) {
	switch runtime.GOOS {
	case "darwin":
		return "open", []string{url}
	case "windows":
		// rundll32 rather than `cmd /c start`, which would need a shell to parse
		// the URL — exactly what this function exists to avoid.
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		return "xdg-open", []string{url}
	}
}
