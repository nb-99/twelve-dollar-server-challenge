package api

import "github.com/gofiber/fiber/v3"

// Response bodies are structs because the spec fixes their key order. Post
// bodies come from the store as JSON already.
type (
	likeResponse struct {
		Liked        bool  `json:"liked"`
		AlreadyLiked bool  `json:"already_liked"`
		PostID       int64 `json:"post_id"`
	}
	healthResponse struct {
		Status  string `json:"status"`
		DB      string `json:"db"`
		UptimeS int64  `json:"uptime_s"`
	}
	degradedResponse struct {
		Status string `json:"status"`
		DB     string `json:"db"`
		Error  string `json:"error"`
	}
	errorResponse struct {
		Error string `json:"error"`
	}
)

// sendJSON sends body, which is JSON already, with the Content-Type of c.JSON.
func sendJSON(c fiber.Ctx, status int, body []byte) error {
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	return c.Status(status).Send(body)
}
