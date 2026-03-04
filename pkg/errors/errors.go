package errors

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

const (
	CodeRateLimitExhausted = "RATE_LIMIT_EXHAUSTED"
	CodeStaleData          = "STALE_DATA"
	CodeEntitlementDenied  = "ENTITLEMENT_DENIED"
	CodeSourceTimeout      = "SOURCE_TIMEOUT"
	CodeInvalidQuery       = "INVALID_QUERY"
)

var (
	ErrRateLimitExhausted = errors.New(CodeRateLimitExhausted)
	ErrStaleData          = errors.New(CodeStaleData)
	ErrEntitlementDenied  = errors.New(CodeEntitlementDenied)
	ErrSourceTimeout      = errors.New(CodeSourceTimeout)
	ErrInvalidQuery       = errors.New(CodeInvalidQuery)
)

type QueryError struct {
	Code       string        `json:"code"`
	Message    string        `json:"message"`
	RetryAfter time.Duration `json:"-"`
	Source     string        `json:"source,omitempty"`
	Cause      error         `json:"-"`
}

func (e *QueryError) Error() string {
	if e.Source == "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("%s (%s): %s", e.Code, e.Source, e.Message)
}

func (e *QueryError) Unwrap() error {
	switch e.Code {
	case CodeRateLimitExhausted:
		return ErrRateLimitExhausted
	case CodeStaleData:
		return ErrStaleData
	case CodeEntitlementDenied:
		return ErrEntitlementDenied
	case CodeSourceTimeout:
		return ErrSourceTimeout
	case CodeInvalidQuery:
		return ErrInvalidQuery
	default:
		return e.Cause
	}
}

func (e *QueryError) HTTPStatus() int {
	switch e.Code {
	case CodeRateLimitExhausted:
		return http.StatusTooManyRequests
	case CodeStaleData:
		return http.StatusConflict
	case CodeEntitlementDenied:
		return http.StatusForbidden
	case CodeSourceTimeout:
		return http.StatusGatewayTimeout
	case CodeInvalidQuery:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func New(code, message, source string, retryAfter time.Duration, cause error) *QueryError {
	return &QueryError{
		Code:       code,
		Message:    message,
		Source:     source,
		RetryAfter: retryAfter,
		Cause:      cause,
	}
}
