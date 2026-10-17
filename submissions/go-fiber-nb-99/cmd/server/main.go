// Command server serves the challenge's feed API on HOST:PORT from the SQLite
// database at SQLITE_PATH.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/gofiber/fiber/v3"

	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/api"
	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/auth"
	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/config"
	"github.com/nb-99/twelve-dollar-server-challenge/submissions/go-fiber-nb-99/internal/store"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	tuneGC()
	if cfg.PprofAddr != "" {
		if err := servePprof(cfg.PprofAddr); err != nil {
			return err
		}
	}
	db, err := store.Open(cfg.SQLitePath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	app := api.New(db, auth.New([]byte(cfg.JWTSecret)))
	log.Printf("listening on %s", cfg.Addr())
	return app.Listen(cfg.Addr(), fiber.ListenConfig{
		DisableStartupMessage: true,
		GracefulContext:       ctx,
	})
}

// tuneGC sets garbage-collector defaults for the challenge VPS (1 vCPU, 2 GB)
// unless GOGC or GOMEMLIMIT is set. The live heap is small, so GOGC=400 lets
// the heap grow to five times it between collections instead of twice, and
// collections run about a quarter as often. The 1.5 GiB soft limit counts
// goroutine stacks too, about 8 KiB per open connection, and makes collections
// run earlier as it nears; about 500 MB is left to the OS and SQLite, whose
// allocations and memory-mapped pages Go does not count.
func tuneGC() {
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(400)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(1536 << 20)
	}
}

// servePprof serves net/http/pprof's handlers, which it registers on
// http.DefaultServeMux, on addr in the background.
func servePprof(addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("pprof: %w", err)
	}
	log.Printf("pprof on http://%s/debug/pprof/", listener.Addr())
	go func() { log.Printf("pprof: %v", http.Serve(listener, nil)) }()
	return nil
}
