// Package testdb gives integration tests their own freshly migrated
// PostgreSQL database.
//
// Set LUX_TEST_DATABASE_URL to a server where the role may CREATE DATABASE,
// for example postgres://lux:lux-dev@127.0.0.1:55432/lux?sslmode=disable.
// Without it, integration tests are skipped, unless LUX_REQUIRE_DB=1, in
// which case they fail (CI sets this so a missing database is never silent).
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-gitagent/lux/internal/db"
)

// New creates a new database, applies all migrations, and drops it when the
// test ends. It returns the pool and the database URL.
func New(t testing.TB) (*pgxpool.Pool, string) {
	t.Helper()
	base := os.Getenv("LUX_TEST_DATABASE_URL")
	if base == "" {
		if os.Getenv("LUX_REQUIRE_DB") == "1" {
			t.Fatal("LUX_REQUIRE_DB=1 but LUX_TEST_DATABASE_URL is not set")
		}
		t.Skip("integration test: set LUX_TEST_DATABASE_URL to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to test server: %v", err)
	}
	defer admin.Close(context.Background())

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "lux_test_" + hex.EncodeToString(b)
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	dbURL := u.String()

	pool, err := db.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Logf("cleanup: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{name}.Sanitize()+` WITH (FORCE)`); err != nil {
			t.Logf("drop test database %s: %v", name, err)
		}
	})
	return pool, dbURL
}
