package config

import "testing"

func TestFromEnv(t *testing.T) {
	t.Setenv("SQLITE_PATH", "/data/feed.db")
	t.Setenv("JWT_SECRET", "secret")
	t.Setenv("HOST", "")
	t.Setenv("PORT", "")
	t.Setenv("PPROF_ADDR", "")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{SQLitePath: "/data/feed.db", JWTSecret: "secret", Host: "127.0.0.1", Port: "3000"}
	if c != want || c.Addr() != "127.0.0.1:3000" {
		t.Errorf("got %+v (%s), want %+v", c, c.Addr(), want)
	}

	t.Setenv("HOST", "0.0.0.0")
	t.Setenv("PORT", "80")
	t.Setenv("PPROF_ADDR", "127.0.0.1:6060")
	if c, _ := FromEnv(); c.Addr() != "0.0.0.0:80" || c.PprofAddr != "127.0.0.1:6060" {
		t.Errorf("got address %s and pprof address %q, want 0.0.0.0:80 and 127.0.0.1:6060", c.Addr(), c.PprofAddr)
	}
}

func TestFromEnvRequiresDatabaseAndSecret(t *testing.T) {
	for _, missing := range []string{"SQLITE_PATH", "JWT_SECRET"} {
		t.Run(missing, func(t *testing.T) {
			t.Setenv("SQLITE_PATH", "/data/feed.db")
			t.Setenv("JWT_SECRET", "secret")
			t.Setenv(missing, "")
			if _, err := FromEnv(); err == nil {
				t.Errorf("FromEnv succeeded without %s", missing)
			}
		})
	}
}
