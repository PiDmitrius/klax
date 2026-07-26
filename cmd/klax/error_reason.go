package main

import (
	"context"
	"errors"
)

const (
	turnErrAborted            = "aborted"
	turnErrAttachmentsMissing = "attachments-missing"
	turnErrRunStartFailed     = "run-start-failed"
	turnErrBackendFailed      = "backend-failed"
	turnErrAuditStartFailed   = "audit-start-failed"
	turnWarnAuditFinishFailed = "audit-finish-failed"
)

func turnErrorReason(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return turnErrAborted
	}
	switch err.Error() {
	case turnErrAttachmentsMissing, turnErrRunStartFailed, turnErrAuditStartFailed:
		return err.Error()
	default:
		return turnErrBackendFailed
	}
}
