package domain

// RequestRetryError carries a usecase decision for repeating the original request.
// It never describes a new operation, request safety or an idempotency key's age.
type RequestRetryError struct {
	cause   error
	allowed bool
}

func (e *RequestRetryError) Error() string { return e.cause.Error() }
func (e *RequestRetryError) Unwrap() error { return e.cause }

// RequestRetryPermission reports only the decision made at the actual refusal phase.
func (e *RequestRetryError) RequestRetryPermission() bool { return e.allowed }

// WithRequestRetry attaches an explicit phase decision; an unclassified error stays unmarked.
func WithRequestRetry(err error, allowed bool) error {
	if err == nil {
		return nil
	}
	return &RequestRetryError{cause: err, allowed: allowed}
}
