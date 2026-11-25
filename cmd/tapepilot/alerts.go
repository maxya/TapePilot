package main

import (
	"encoding/json"
	"errors"
	"io"
	"strconv"

	linux "github.com/maxya/TapePilot/internal/tape/linux"
)

// alertsCommand interprets an ordinary captured file. It has no runtime, drive
// or SG handle and runs before stateful application startup.
func alertsCommand(args []string, out io.Writer) error {
	if len(args) != 8 || args[0] != "tape-alerts" || args[2] != "--layout" || args[4] != "--qualification-ref" || args[6] != "--pdt" {
		return linux.ErrTapeAlertProfile
	}
	pdt, e := strconv.ParseUint(args[7], 10, 8)
	if e != nil {
		return linux.ErrTapeAlertProfile
	}
	profile := linux.TapeAlertCaptureProfile{Layout: args[3], QualificationRef: args[5], PeripheralDeviceType: uint8(pdt)}
	report, readErr := linux.ReadCapturedTapeAlerts(args[1], profile)
	return errors.Join(readErr, json.NewEncoder(out).Encode(report))
}
