package spot

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestSQLiteFleetCompareAndSwapSurvivesRestartWithoutTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.db")
	a, e := OpenSQLite(path, "test")
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	one, _ := a.Load(ctx)
	stale, _ := a.Load(ctx)
	one.Allocations["one"] = Allocation{State: "launching", LaunchUncertain: true}
	if e = a.Save(ctx, &one); e != nil {
		t.Fatal(e)
	}
	if e = a.Save(ctx, &stale); !errors.Is(e, ErrConflict) {
		t.Fatal("stale state overwrote live reservation", e)
	}
	a.Close()
	b, e := OpenSQLite(path, "test")
	if e != nil {
		t.Fatal(e)
	}
	defer b.Close()
	state, e := b.Load(ctx)
	if e != nil || !state.Allocations["one"].LaunchUncertain {
		t.Fatal("unknown launch slot disappeared on restart", state, e)
	}
}
