package quota

import (
	"context"
	"testing"
	"time"
)

func TestMemoryCounterRollsAtUTCMidnight(t *testing.T) {
	c := New(3, "")
	now := time.Date(2026, 10, 5, 23, 59, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }
	ctx := context.Background()
	if ok, used := c.Reserve(ctx, 2); !ok || used != 2 {
		t.Fatalf("%v %d", ok, used)
	}
	if ok, used := c.Reserve(ctx, 2); ok || used != 2 {
		t.Fatalf("should refuse: %v %d", ok, used)
	}
	if ok, _ := c.Reserve(ctx, 1); !ok {
		t.Fatal("exact fit should pass")
	}
	now = now.Add(2 * time.Minute)
	if c.Used(ctx) != 0 {
		t.Fatal("expected reset after midnight")
	}
	if NextReset(now) != time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC) {
		t.Fatalf("next reset %v", NextReset(now))
	}
	if New(0, "").Budget != 0 {
		t.Fatal()
	}
	if ok, _ := New(0, "").Reserve(ctx, 1000); !ok {
		t.Fatal("budget 0 disables the check")
	}
}
