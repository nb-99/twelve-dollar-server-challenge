package api

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// apiError is an error response with a status and an exact message.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return e.message }

var (
	errInvalidPostID = &apiError{fiber.StatusBadRequest, "invalid post id"}
	errPostNotFound  = &apiError{fiber.StatusNotFound, "post not found"}
	errMalformedJSON = &apiError{fiber.StatusBadRequest, "malformed JSON body"}
	errBodyRequired  = &apiError{fiber.StatusBadRequest, "body is required"}
	errBodyTooLong   = &apiError{fiber.StatusBadRequest, "body must be at most 500 characters"}
)

// handleError writes every error as {"error":"<message>"}. Fiber's own client
// errors, such as an unknown path, keep their status with its standard text.
// Anything else is logged and answered with a 500.
func handleError(c fiber.Ctx, err error) error {
	status, message := fiber.StatusInternalServerError, "internal server error"
	var apiErr *apiError
	var fiberErr *fiber.Error
	switch {
	case errors.As(err, &apiErr):
		status, message = apiErr.status, apiErr.message
	case errors.As(err, &fiberErr) && fiberErr.Code < fiber.StatusInternalServerError:
		status, message = fiberErr.Code, strings.ToLower(http.StatusText(fiberErr.Code))
	default:
		log.Printf("%s %s: %v", c.Method(), c.Path(), err)
	}
	return c.Status(status).JSON(errorResponse{Error: message})
}
