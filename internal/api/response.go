package api

import (
	"net/http"

	"github.com/go-playground/validator/v10"
)

type SuccessResponse struct {
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data"`
}

type ErrorDetail struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type Error struct {
	// Code is an optional stable, machine-readable error code (e.g.
	// INPUT_NOT_FOUND). It lets clients/acceptance distinguish lifecycle
	// errors without parsing the human message or receiving a blanket 500.
	Code    string        `json:"code,omitempty"`
	Message string        `json:"message"`
	Details []ErrorDetail `json:"details,omitempty"`
}

type ErrorResponse struct {
	Error `json:"error"`
}

type StatusCode int

// HandleValidationError validate and handle errors of structs that has `validator/v10` rules
func HandleValidationError(err error) (StatusCode, *ErrorResponse) {
	validationErrors, ok := err.(validator.ValidationErrors)
	if !ok {
		return 0, nil
	}

	details := make([]ErrorDetail, 0, len(validationErrors))

	for _, fieldErr := range err.(validator.ValidationErrors) {
		details = append(details, ErrorDetail{
			Reason:  fieldErr.Field(),
			Message: fieldErr.ActualTag(),
		})
	}

	return http.StatusBadRequest, &ErrorResponse{
		Error: Error{
			Message: "Invalid request parameters",
			Details: details,
		},
	}
}
