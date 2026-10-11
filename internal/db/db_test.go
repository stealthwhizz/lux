package db_test

import (
	"context"
	"testing"

	"github.com/open-gitagent/lux/internal/db"
	"github.com/open-gitagent/lux/internal/testdb"
)

func TestMigrateIsIdempotent(t *testing.T) {
	pool, _ := testdb.New(t)
	ctx := context.Background()

	v, err := db.SchemaVersion(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if v != db.LatestVersion() || v == 0 {
		t.Fatalf("schema version %d, latest %d", v, db.LatestVersion())
	}
	applied, err := db.Migrate(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("second migrate applied %v", applied)
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	pool, _ := testdb.New(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, name) VALUES (9999, 'from_the_future.sql')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Migrate(ctx, pool); err == nil {
		t.Fatal("expected an error for a schema newer than the binary")
	}
}

func TestAuditEventsAreAppendOnly(t *testing.T) {
	pool, _ := testdb.New(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO audit_events (org_id, actor_kind, action) VALUES ('org_x', 'system', 'test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_events SET action = 'tampered'`); err == nil {
		t.Fatal("UPDATE on audit_events must fail")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM audit_events`); err == nil {
		t.Fatal("DELETE on audit_events must fail")
	}
}
