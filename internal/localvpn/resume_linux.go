package localvpn

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const resumePathUnit = "local-vpn-kde-resume.path"
const resumePathContents = `[Unit]
Description=Watch for system resume during this graphical session
PartOf=graphical-session.target
After=graphical-session-pre.target

[Path]
PathChanged=/run/local-vpn-kde-resumed

[Install]
WantedBy=graphical-session.target
`

func (a Autostart) prepareResumeUnits(dir int, enabled bool) (bool, error) {
	service, err := a.actionUnit(true)
	if err != nil {
		return false, err
	}
	units := map[string]string{resumePathUnit: a.marker() + resumePathContents, "local-vpn-kde-resume.service": service}
	found := false
	for name := range units {
		old, err := readAt(dir, name)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err != nil {
			return false, err
		}
		if !strings.HasPrefix(string(old), a.marker()) {
			return false, errors.New("resume unit is not owned")
		}
		found = true
	}
	if !enabled && !found {
		return false, nil
	}
	for name, data := range units {
		if err := atomicWrite(dir, name, []byte(data)); err != nil {
			return false, err
		}
	}
	return true, nil
}

var errResumeDisabled = errors.New("resume autostart disabled")

func (a Autostart) resumeGuard(ctx context.Context) error {
	options, err := a.Status(ctx)
	if err != nil {
		return err
	}
	if !options.Resume {
		return errResumeDisabled
	}
	nm := NetworkManager{Base: a.Base, Native: true}
	cap, err := nm.readCapability()
	if err != nil {
		return err
	}
	return nm.checkOtherVPN(ctx, cap.UUID)
}

func (a Autostart) Resume(ctx context.Context) error {
	if os.Geteuid() == 0 || !a.Installed || a.Runtime != "native" {
		return errors.New("resume requires the installed desktop session")
	}
	if err := a.validateBase(); err != nil {
		return err
	}
	options, err := a.Status(ctx)
	if err != nil || !options.Resume {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	result, _ := recordLifecycle(a.Base, "resume-connect", nil, func(out *attemptOutput) ProcessResult {
		readyCtx, stop := context.WithTimeout(ctx, time.Minute)
		defer stop()
		for {
			guardErr := a.resumeGuard(readyCtx)
			if errors.Is(guardErr, errResumeDisabled) || errors.Is(guardErr, ErrOtherVPNActive) {
				return resumeResult(guardErr)
			}
			if guardErr == nil {
				if _, err := nativeEgressReady(readyCtx); err == nil {
					break
				}
			}
			select {
			case <-readyCtx.Done():
				return resumeOutcome("prerequisites-unavailable", 1)
			case <-time.After(3 * time.Second):
			}
		}
		return resumeResult(runNativeDesktop(ctx, a.Base, "start", true, out, a.resumeGuard))
	})
	if result.Code == nil || *result.Code != 0 || result.Reason == "diagnostics-unavailable" {
		return errors.New("resume connection failed; inspect private diagnostics")
	}
	return nil
}

func resumeResult(err error) ProcessResult {
	switch {
	case err == nil:
		return resumeOutcome("ok", 0)
	case errors.Is(err, errResumeDisabled):
		return resumeOutcome("disabled", 0)
	case errors.Is(err, ErrOtherVPNActive):
		return resumeOutcome("other-vpn-active", 0)
	default:
		return resumeOutcome("resume-connect-failed", 1)
	}
}

func resumeOutcome(reason string, code int) ProcessResult {
	return ProcessResult{Reason: reason, Code: &code}
}
