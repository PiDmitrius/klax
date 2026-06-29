package main

import (
	"context"
	"errors"
)

const (
	turnErrAborted            = "aborted"
	turnErrAttachmentsMissing = "attachments-missing"
	turnErrRunStartFailed     = "run-start-failed"
)

func turnErrorReason(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return turnErrAborted
	}
	return err.Error()
}
