//go:build linux

package localvpn

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// nativeSetupFailureStages maps the fixed setup error messages to stable,
// non-secret stage codes. Unlisted errors report "other"; error text itself
// (which may be caller-influenced) is never emitted.
var nativeSetupFailureStages = []struct{ message, stage string }{
	{"native services must be stopped", "services-not-stopped"},
	{"native failed services could not be reset", "reset-failed"},
	{"native configuration generation failed", "generate"},
	{"native setup inhibitor", "inhibitor"},
	{"native setup lock unavailable", "lock"},
	{"native setup blocked by a package transaction", "package-transaction"},
	{"native client profile staging failed", "profile-stage"},
	{"native configuration publication failed", "publish"},
	{"native configuration was committed", "publish-committed"},
	{"native configuration committed but profile commit", "profile-commit"},
	{"native client profile commit failed", "profile-commit"},
	{"native server selection failed", "select-server"},
	{"native authorization enrollment failed", "enroll"},
	{"native desktop finalization failed", "desktop-finalize"},
	{"native configuration installed but profile import failed", "profile-import"},
	{"native configuration installed but private subscription copy failed", "subscription-copy"},
	{"native setup completed but inhibitor cleanup failed", "inhibitor-cleanup"},
	{"native setup canceled", "canceled"},
	{"native service identity", "identity"},
	{"native authorization group", "identity"},
	{"installed native assets", "assets"},
	{"native configuration caller", "caller"},
	{"native configuration requires the authorized installed helper", "helper"},
}

func nativeSetupFailureStage(err error) string {
	message := err.Error()
	for _, entry := range nativeSetupFailureStages {
		if strings.HasPrefix(message, entry.message) {
			return entry.stage
		}
	}
	return "other"
}

func reportNativeSetupFailure(w io.Writer, err error) {
	if err != nil {
		fmt.Fprintf(w, "native_setup_failure=%s\n", nativeSetupFailureStage(err))
	}
}

var nativeSetupFailureLine = regexp.MustCompile(`^native_setup_failure=[a-z-]{1,32}$`)

// nativeSetupFailureFilter forwards only exact failure-stage lines.
type nativeSetupFailureFilter struct {
	out io.Writer
	buf []byte
}

func (f *nativeSetupFailureFilter) Write(p []byte) (int, error) {
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			if len(f.buf) > 256 {
				f.buf = nil
			}
			break
		}
		line := f.buf[:i]
		if len(line) <= 64 && nativeSetupFailureLine.Match(line) {
			fmt.Fprintf(f.out, "%s\n", line)
		}
		f.buf = f.buf[i+1:]
	}
	return len(p), nil
}
