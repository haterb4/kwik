package protocol

import (
	"fmt"
)

// KWIK error codes - Path management
const (
	ErrPathExists    = "KWIK_PATH_EXISTS"
	ErrPathNotExists = "KWIK_PATH_NOT_EXISTS"
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
	return NewKwikError(ErrPathNotExists, fmt.Sprintf("path %d does not exist", pathID), nil)
}
func NewInvalidPathIDError(pathID PathID) error {
	return NewKwikError(ErrInvalidPathId, fmt.Sprintf("id %d is invalid path ID", pathID), nil)
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
	ErrDataStreamExists    = "KWIK_DATA_STREAM_EXISTS"
	ErrDataStreamNotExists = "KWIK_DATA_STREAM_NOT_EXISTS"
)

func NewExistStreamError(pathID PathID, streamID StreamID) error {
	return NewKwikError(ErrDataStreamExists, fmt.Sprintf("failed to create data stream %d on path: %d stream allready exists", streamID, pathID), nil)
}
func NewNotExistStreamError(pathID PathID, streamID StreamID) error {
	return NewKwikError(ErrDataStreamNotExists, fmt.Sprintf("stream %d not found on path: %d", streamID, pathID), nil)
}

// Handshake and control errors
const (
	ErrHandshakeFailed      = "KWIK_HANDSHAKE_FAILED"
	ErrHandshakeNonceFailed = "KWIK_HANDSHAKE_NONCE_FAILED"
	ErrFrameTooLarge        = "KWIK_FRAME_TOO_LARGE"
)

func NewHandshakeFailedError(pathID PathID) error {
	return NewKwikError(ErrHandshakeFailed, fmt.Sprintf("handshake failed on path %d", pathID), nil)
}

func NewHandshakeNonceError(pathID PathID, cause error) error {
	return NewKwikError(ErrHandshakeNonceFailed, fmt.Sprintf("failed to generate or validate handshake nonce on path %d", pathID), cause)
}

func NewFrameTooLargeError(frameSize, maxSize int) error {
	return NewKwikError(ErrFrameTooLarge, fmt.Sprintf("frame too large for maxPacketSize: %d > %d", frameSize, maxSize), nil)
}
