type ErrorCategory string

const (
    ErrCategoryRateLimit   ErrorCategory = "RATE_LIMIT"   // retryable
    ErrCategoryServer      ErrorCategory = "SERVER_ERROR" // retryable
    ErrCategoryInvalidReq  ErrorCategory = "INVALID_REQ"  // not retryable
    ErrCategoryAuth        ErrorCategory = "AUTH_ERROR"   // not retryable
	ErrCategoryUnknown     ErrorCategory = "UNKNOWN"      // not retryable
)

type LLMError struct {
    Category ErrorCategory
    Message  string
    RawError error // original error message
}

func (e *LLMError) Error() string {
    return e.Message
}

// checks if the error is retryable
func (e *LLMError) IsRetryable() bool {
    return e.Category == ErrCategoryRateLimit || e.Category == ErrCategoryServer
}