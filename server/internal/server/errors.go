package server

import (
	"errors"
	"fmt"
	"net/http"
)

// Code values match gRPC status codes, which Nakama clients and Lua modules
// see in error responses.
type Code int

const (
	CodeOK                 Code = 0
	CodeCanceled           Code = 1
	CodeUnknown            Code = 2
	CodeInvalidArgument    Code = 3
	CodeDeadlineExceeded   Code = 4
	CodeNotFound           Code = 5
	CodeAlreadyExists      Code = 6
	CodePermissionDenied   Code = 7
	CodeResourceExhausted  Code = 8
	CodeFailedPrecondition Code = 9
	CodeAborted            Code = 10
	CodeOutOfRange         Code = 11
	CodeUnimplemented      Code = 12
	CodeInternal           Code = 13
	CodeUnavailable        Code = 14
	CodeDataLoss           Code = 15
	CodeUnauthenticated    Code = 16
)

func (c Code) HTTPStatus() int {
	switch c {
	case CodeOK:
		return http.StatusOK
	case CodeCanceled:
		return 499
	case CodeInvalidArgument, CodeFailedPrecondition, CodeOutOfRange:
		return http.StatusBadRequest
	case CodeDeadlineExceeded:
		return http.StatusGatewayTimeout
	case CodeNotFound:
		return http.StatusNotFound
	case CodeAlreadyExists, CodeAborted:
		return http.StatusConflict
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeResourceExhausted:
		return http.StatusTooManyRequests
	case CodeUnimplemented:
		return http.StatusNotImplemented
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

type StatusError struct {
	Code    Code
	Message string
	Cause   error
}

func (e *StatusError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *StatusError) Unwrap() error { return e.Cause }

func statusErr(code Code, msg string) *StatusError { return &StatusError{Code: code, Message: msg} }

func codeOf(err error) Code {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return CodeInternal
}

func messageOf(err error) string {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Message
	}
	return "Internal server error"
}

var (
	errStorageRejectedVersion    = statusErr(CodeFailedPrecondition, "Storage write rejected - version check failed.")
	errStorageRejectedPermission = statusErr(CodeInvalidArgument, "Storage write rejected - permission denied.")
	errUserNotFound              = statusErr(CodeNotFound, "User account not found.")
	errAccountDisabled           = statusErr(CodePermissionDenied, "User account banned.")
	errInvalidCredentials        = statusErr(CodeUnauthenticated, "Invalid credentials.")
	errUsernameInUse             = statusErr(CodeAlreadyExists, "Username is already in use.")
	errMatchNotFound             = statusErr(CodeNotFound, "Match not found.")
)
