// Package config reads the server's runtime contract from the environment.
package config

import (
	"errors"
	"net"
	"os"
)

type Config struct {
	SQLitePath string
	JWTSecret  string
	Host       string
	Port       string
	// PprofAddr is where net/http/pprof listens; empty disables it.
	PprofAddr string
}

// Addr is the HOST:PORT address to listen on.
func (c Config) Addr() string {
	return net.JoinHostPort(c.Host, c.Port)
}

// FromEnv reads SQLITE_PATH and JWT_SECRET, which are required, HOST and
// PORT, which default to 127.0.0.1:3000, and the optional PPROF_ADDR.
func FromEnv() (Config, error) {
	c := Config{
		SQLitePath: os.Getenv("SQLITE_PATH"),
		JWTSecret:  os.Getenv("JWT_SECRET"),
		Host:       envOr("HOST", "127.0.0.1"),
		Port:       envOr("PORT", "3000"),
		PprofAddr:  os.Getenv("PPROF_ADDR"),
	}
	if c.SQLitePath == "" || c.JWTSecret == "" {
		return Config{}, errors.New("SQLITE_PATH and JWT_SECRET are required")
	}
	return c, nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
