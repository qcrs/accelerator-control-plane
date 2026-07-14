package contracts

import (
	"errors"
	"fmt"
)

type ErrorKind string

const (
	ErrorKindUnschedulable ErrorKind = "Unschedulable"
	ErrorKindConflict      ErrorKind = "Conflict"
	ErrorKindTransient     ErrorKind = "Transient"
	ErrorKindUnsupported   ErrorKind = "Unsupported"
	ErrorKindInvalid       ErrorKind = "Invalid"
)

type DomainError struct {
	Kind    ErrorKind
	Op      string
	Message string
	Cause   error
}

func (e *DomainError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Op, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Op, e.Message)
}

func (e *DomainError) Unwrap() error { return e.Cause }

func NewError(kind ErrorKind, op, message string) error {
	return &DomainError{Kind: kind, Op: op, Message: message}
}

func WrapError(kind ErrorKind, op, message string, cause error) error {
	return &DomainError{Kind: kind, Op: op, Message: message, Cause: cause}
}

func IsErrorKind(err error, kind ErrorKind) bool {
	var domainErr *DomainError
	return errors.As(err, &domainErr) && domainErr.Kind == kind
}
