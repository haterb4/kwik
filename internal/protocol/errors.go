package protocol

import (
	"fmt"
)

// KWIK error codes - Path management
const (
	ErrPathExists    = "KWIK_PATH_EXISTS"
	ErrInvalidPathId = "KWIK_PATH_INVALID_ID"
)

type KwikError struct {
	Code    string
	Message string
	Cause   error
}

func (e *KwikError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s (caused by: %v)", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *KwikError) Unwrap() error {
	return e.Cause
}

func NewKwikError(code, message string, cause error) *KwikError {
	return &KwikError{
		Code:    code,
		Message: message,
		Cause:   cause,
	}
}

func NewPathNotExistsError(pathID PathID) error {
	return NewKwikError(ErrPathExists, fmt.Sprintf("path %d allready exists", pathID), nil)
}
func NewInvalidPathIDError(pathID PathID) error {
	return NewKwikError(ErrPathExists, fmt.Sprintf("id %d is invalid path ID", pathID), nil)
}

// KWIK error codes - Control plane
const (
	ErrControlStreamFails = "KWIK_CONTROL_STREAM_CREATION_FAILED"
)

func NewControlStreamCreationError(pathID PathID) error {
	return NewKwikError(ErrControlStreamFails, fmt.Sprintf("failed to create control stream on path: %d", pathID), nil)
}

// KWIK error codes - Data plane
const (
	ErrDataStreamExists = "KWIK_DATA_STREAM_EXISTS"
)

func NewExistStremError(pathID PathID, streamID StreamID) error {
	return NewKwikError(ErrControlStreamFails, fmt.Sprintf("failed to create data stream %d on path: %d stream allready exists", streamID, pathID), nil)
}
