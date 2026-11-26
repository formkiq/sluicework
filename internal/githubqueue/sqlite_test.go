package githubqueue

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestSQLiteQueuePersistsAndRejectsStaleWrites(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r := request("one", High, time.Now())
	r.Kind = "site"
	r.RepositoryID = 7
	if err = store.Save(ctx, &r); err != nil {
		t.Fatal(err)
	}
	stale := r
	r.State = "waiting"
	if err = store.Save(ctx, &r); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(store.Save(ctx, &stale), ErrConflict) {
		t.Fatal("stale write accepted")
	}
	duplicate := request("two", Low, time.Now())
	duplicate.Kind = "site"
	duplicate.RepositoryID = 7
	if !errors.Is(store.Save(ctx, &duplicate), ErrExists) {
		t.Fatal("duplicate parent accepted")
	}
	loaded, err := store.Get(ctx, r.Key())
	if err != nil || loaded.State != "waiting" {
		t.Fatalf("%+v %v", loaded, err)
	}
	if _, err = store.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
func TestSQLiteDispatcherLeaseHasAnOwnerAndExpires(t *testing.T) {
	db, _ := sql.Open("sqlite", filepath.Join(t.TempDir(), "queue.db"))
	defer db.Close()
	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	if ok, err := store.Lease(ctx, "one", now, time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := store.Lease(ctx, "two", now, time.Minute); err != nil || ok {
		t.Fatal("second owner acquired live lease", ok, err)
	}
	if err = store.Release(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.Lease(ctx, "two", now, time.Minute); ok {
		t.Fatal("another owner released the lock")
	}
	if ok, err := store.Lease(ctx, "two", now.Add(2*time.Minute), time.Minute); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err = store.Release(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := store.Lease(ctx, "three", now.Add(2*time.Minute), time.Minute); ok {
		t.Fatal("stale owner released a newer lease")
	}
}
