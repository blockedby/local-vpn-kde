//go:build linux

package main

import (
	"errors"
	"io"
	"os"
)

const (
	exitFailure = 1
	exitUsage   = 2
	exitOpen    = 3
)

func main() {
	os.Exit(dispatch(os.Args[1:], newProductionSupervisor(), os.Stdout))
}

// dispatch intentionally emits no diagnostics. The only output channel is the
// bounded final response and validated measurement progress of a fixed operation.
func dispatch(args []string, supervisor *supervisor, stdout io.Writer) int {
	if len(args) == 0 {
		return exitUsage
	}
	switch args[0] {
	case "prepare":
		if len(args) < 3 || len(args) > 5 || (len(args) == 5 && args[2] != "availability" && args[2] != "check-batch") {
			return exitUsage
		}
		if err := supervisor.prepare(args[1:]); err != nil {
			return exitFailure
		}
		return 0
	case "run":
		speedProgress := len(args) == 5 && args[2] == "speed" && args[4] == "--progress"
		if speedProgress {
			args = args[:4]
		}
		if len(args) < 3 || len(args) > 5 || (len(args) == 5 && args[2] != "availability" && args[2] != "check-batch") {
			return exitUsage
		}
		var progress []io.Writer
		if args[2] == "check-batch" || speedProgress || (args[2] == "speed" && os.Getenv("VPNKIT_SPEED_PROGRESS") == "1") {
			progress = []io.Writer{stdout}
		}
		result, err := supervisor.run(args[1:], progress...)
		if err != nil {
			return exitFailure
		}
		if len(result.output) > 0 {
			if _, err := stdout.Write(result.output); err != nil {
				return exitFailure
			}
		}
		return result.exitCode
	case "cancel":
		if len(args) != 2 {
			return exitUsage
		}
		if err := supervisor.cancel(args[1]); err != nil {
			return exitFailure
		}
		return 0
	case "verify":
		if len(args) != 2 {
			return exitUsage
		}
		err := supervisor.verify(args[1])
		if errors.Is(err, errOpen) {
			return exitOpen
		}
		if err != nil {
			return exitFailure
		}
		return 0
	default:
		return exitUsage
	}
}
