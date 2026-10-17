package api

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"

	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/auth"
	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/store"
)

const maxBodyChars = 500

func (h *handlers) health(c fiber.Ctx) error {
	if err := h.store.Ping(c.Context()); err != nil {
		return c.Status(fiber.StatusServiceUnavailable).
			JSON(degradedResponse{Status: "degraded", DB: "unreachable", Error: err.Error()})
	}
	uptime := int64(time.Since(h.started).Seconds())
	return c.JSON(healthResponse{Status: "ok", DB: "ok", UptimeS: uptime})
}

func (h *handlers) feed(c fiber.Ctx) error {
	body, err := h.store.FeedJSON(c.Context())
	if err != nil {
		return err
	}
	return sendJSON(c, fiber.StatusOK, body)
}

func (h *handlers) getPost(c fiber.Ctx) error {
	id, err := postID(c)
	if err != nil {
		return err
	}
	body, err := h.store.PostJSON(c.Context(), id)
	if errors.Is(err, store.ErrPostNotFound) {
		return errPostNotFound
	}
	if err != nil {
		return err
	}
	return sendJSON(c, fiber.StatusOK, body)
}

func (h *handlers) createPost(c fiber.Ctx) error {
	user, err := h.authenticate(c)
	if err != nil {
		return err
	}
	text, err := parsePostBody(c.Body())
	if err != nil {
		return err
	}
	body, err := h.store.CreatePostJSON(c.Context(), user.ID, user.Name, text)
	if err != nil {
		return err
	}
	return sendJSON(c, fiber.StatusCreated, body)
}

func (h *handlers) likePost(c fiber.Ctx) error {
	user, err := h.authenticate(c)
	if err != nil {
		return err
	}
	id, err := postID(c)
	if err != nil {
		return err
	}
	alreadyLiked, err := h.store.Like(c.Context(), user.ID, id)
	if errors.Is(err, store.ErrPostNotFound) {
		return errPostNotFound
	}
	if err != nil {
		return err
	}
	status := fiber.StatusCreated
	if alreadyLiked {
		status = fiber.StatusOK
	}
	return c.Status(status).JSON(likeResponse{Liked: true, AlreadyLiked: alreadyLiked, PostID: id})
}

func (h *handlers) authenticate(c fiber.Ctx) (auth.User, error) {
	user, err := h.auth.Authenticate(c.Get(fiber.HeaderAuthorization))
	if err != nil {
		return auth.User{}, &apiError{fiber.StatusUnauthorized, err.Error()}
	}
	return user, nil
}

// postID parses the :id parameter, which must be a positive decimal integer
// without a sign.
func postID(c fiber.Ctx) (int64, error) {
	text := c.Params("id")
	if text == "" || strings.Trim(text, "0123456789") != "" {
		return 0, errInvalidPostID
	}
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil || id <= 0 {
		return 0, errInvalidPostID
	}
	return id, nil
}

// parsePostBody returns the trimmed "body" string of a JSON request body.
func parsePostBody(raw []byte) (string, error) {
	// Decoding into a struct would match "Body" case-insensitively and fail on
	// non-string values, which must both report a missing body.
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", errMalformedJSON
	}
	object, _ := payload.(map[string]any)
	body, _ := object["body"].(string)
	body = strings.TrimSpace(body)
	if body == "" {
		return "", errBodyRequired
	}
	// Characters are code points, as in the schema's length(body) check.
	if utf8.RuneCountInString(body) > maxBodyChars {
		return "", errBodyTooLong
	}
	return body, nil
}
