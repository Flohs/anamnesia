package store

import (
	"context"
	"os"
	"testing"

	"github.com/flohs/anamnesia/pkg/anamnesia"
)

func TestDeleteUserCascadesItsMemory(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	uid, err := st.EnsureUser(ctx, "eval-cascade-test")
	if err != nil {
		t.Fatal(err)
	}
	scope := anamnesia.Scope{UserID: uid}
	if err := st.UpsertFact(ctx, &anamnesia.Fact{
		Scope: scope, Key: "eval.cascade", Value: map[string]any{"v": 1},
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := st.DeleteUser(ctx, "eval-cascade-test")
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if !deleted {
		t.Fatal("DeleteUser reported no row deleted")
	}
	if _, ok, err := st.LookupUser(ctx, "eval-cascade-test"); err != nil || ok {
		t.Errorf("user still resolves after delete (ok=%v, err=%v)", ok, err)
	}
	var n int
	if err := st.Pool.QueryRow(ctx,
		`SELECT count(*) FROM facts WHERE user_id = $1`, uid).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d facts survived the user delete; the cascade did not fire", n)
	}
}

func TestDeleteUserThatDoesNotExistIsNotAnError(t *testing.T) {
	dsn := os.Getenv("ANAMNESIA_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ANAMNESIA_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	deleted, err := st.DeleteUser(ctx, "no-such-eval-user-ever")
	if err != nil {
		t.Errorf("deleting an absent user should be a no-op, got %v", err)
	}
	if deleted {
		t.Error("reported a deletion that cannot have happened")
	}
}
