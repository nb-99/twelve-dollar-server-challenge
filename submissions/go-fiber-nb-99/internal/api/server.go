// Package api serves the challenge's HTTP API with Fiber.
package api

import (
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/auth"
	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/store"
)

type handlers struct {
	store   *store.Store
	auth    *auth.Authenticator
	started time.Time
}

// New returns a Fiber app with the five routes of SPEC.md.
func New(s *store.Store, a *auth.Authenticator) *fiber.App {
	h := &handlers{store: s, auth: a, started: time.Now()}
	app := fiber.New(fiber.Config{
		ErrorHandler: handleError,
		// Most keep-alive connections are idle between requests. Releasing
		// their read and write buffers in the meantime costs some CPU but
		// keeps per-connection memory low with tens of thousands of clients.
		ReduceMemoryUsage: true,
	})
	app.Get("/health", h.health)
	app.Get("/feed", h.feed)
	app.Get("/posts/:id", h.getPost)
	app.Post("/posts", h.createPost)
	app.Post("/posts/:id/like", h.likePost)
	return app
}
