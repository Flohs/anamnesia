package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// freshDatabase creates a throwaway database and returns its DSN.
//
// Migration races only exist on an empty database: against one that is
// already migrated, goose reads its version table and does nothing, which
// is why this never reproduces against a long-lived local test database
// and always reproduces on CI.
func freshDatabase(t *testing.T) string {
	t.Helper()
	admin := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if admin == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	name := "anamnesia_migrate_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")

	ctx := context.Background()
	adminStore, err := Open(ctx, admin)
	if err != nil {
		t.Fatalf("open admin: %v", err)
	}
	defer adminStore.Close()
	if _, err := adminStore.Pool.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", name)); err != nil {
		t.Skipf("cannot create a throwaway database (%v); skipping", err)
	}
	t.Cleanup(func() {
		st, err := Open(context.Background(), admin)
		if err != nil {
			return
		}
		defer st.Close()
		_, _ = st.Pool.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name))
	})

	u.Path = "/" + name
	return u.String()
}

// TestConcurrentMigrateIsSafe covers the race that makes CI red while every
// local run is green.
//
// Migrations are DDL and goose serialises nothing, so two processes
// migrating the same empty database collide: one creates an index or a
// type the other is halfway through creating, and the loser reports
// "already exists" from the middle of a migration file. `go test ./...`
// runs packages as concurrent processes against one database, which is
// exactly this shape, and `anamnesia serve` migrating at boot while
// someone runs `anamnesia migrate` by hand is the same thing in
// production.
func TestConcurrentMigrateIsSafe(t *testing.T) {
	dsn := freshDatabase(t)
	ctx := context.Background()

	const racers = 4
	var wg sync.WaitGroup
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := Open(ctx, dsn)
			if err != nil {
				errs[i] = err
				return
			}
			defer st.Close()
			<-start // line them up so they really do collide
			errs[i] = st.Migrate(ctx)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("migration %d failed: %v", i, err)
		}
	}
}
