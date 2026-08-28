// Package winservice bridges the app's lifecycle to the Windows Service
// Control Manager: runs as a proper service when launched by the SCM, or as
// a plain foreground process otherwise (svc.IsWindowsService() tells them
// apart) — same RunFunc either way. Also provides install/uninstall/start/
// stop/status management via SCM (see manage.go).
package winservice

import (
	"context"
	"os"
	"os/signal"

	"golang.org/x/sys/windows/svc"
)

// RunFunc is the application's entry point: it must run until ctx is
// cancelled and then return promptly (a few seconds at most — the SCM times
// out waiting for a service to stop).
type RunFunc func(ctx context.Context) error

// PermanentError, when returned by a RunFunc, tells the service host that
// restarting the process won't help (e.g. invalid config) — the host reports
// a clean stop (Win32ExitCode 0) so Windows recovery actions don't trigger an
// infinite restart loop on something only a human can fix. See
// Service.SetRecoveryActionsOnNonCrashFailures in manage.go: recovery is
// enabled even for non-crash (clean, nonzero-exit-code) stops, specifically
// so genuine runtime failures DO get restarted — PermanentError is the one
// deliberate exception.
type PermanentError struct {
	Err error
}

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// IsWindowsService reports whether the current process was launched by the
// Windows Service Control Manager.
func IsWindowsService() (bool, error) {
	return svc.IsWindowsService()
}

// Run runs the app: under the SCM, as a proper Windows service (name is the
// registered service name); otherwise, in the foreground, stopped by
// Ctrl+C/os.Interrupt.
func Run(name string, run RunFunc) error {
	isService, err := IsWindowsService()
	if err != nil {
		return err
	}

	if isService {
		return svc.Run(name, &handler{run: run, name: name})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return run(ctx)
}
