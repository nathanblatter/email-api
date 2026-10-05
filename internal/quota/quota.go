// Package quota tracks recipients sent per UTC day against the relay's
// daily cap. Redis is the shared source of truth (other services can read or
// increment the same key); if Redis is unreachable the counter degrades to
// in-process so sending never blocks on a side system.
package quota

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const keyPrefix = "email:quota:"

func Key(t time.Time) string { return keyPrefix + t.UTC().Format("2006-01-02") }

// NextReset is the next UTC midnight after t.
func NextReset(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
}

type Counter struct {
	Budget int
	rdb    *redis.Client
	// Now is the clock; tests override it to cross a UTC midnight.
	Now func() time.Time

	mu     sync.Mutex
	memDay string
	memN   int
}

// New returns a counter; redisURL may be empty for in-memory only.
func New(budget int, redisURL string) *Counter {
	c := &Counter{Budget: budget, Now: time.Now}
	if redisURL != "" {
		if opt, err := redis.ParseURL(redisURL); err != nil {
			slog.Warn("quota: bad REDIS_URL, using in-memory counter", "err", err)
		} else {
			c.rdb = redis.NewClient(opt)
		}
	}
	return c
}

// Used returns how many recipients have been counted today.
func (c *Counter) Used(ctx context.Context) int {
	if c.rdb != nil {
		n, err := c.rdb.Get(ctx, Key(c.Now())).Int()
		if err == nil {
			return n
		}
		if err != redis.Nil {
			slog.Warn("quota: redis read failed, using in-memory count", "err", err)
		} else {
			return 0
		}
	}
	return c.memUsed()
}

// Reserve atomically adds n to today's count if it fits within the budget.
// It returns false (and leaves the count unchanged) when the budget would be
// exceeded. Budget <= 0 disables the check.
func (c *Counter) Reserve(ctx context.Context, n int) (ok bool, used int) {
	if c.Budget <= 0 {
		return true, c.Used(ctx)
	}
	if c.rdb != nil {
		key := Key(c.Now())
		res, err := reserveScript.Run(ctx, c.rdb, []string{key}, n, c.Budget, int((48 * time.Hour).Seconds())).Int64()
		if err == nil {
			if res < 0 {
				return false, int(-res - 1)
			}
			return true, int(res)
		}
		slog.Warn("quota: redis reserve failed, using in-memory count", "err", err)
	}
	return c.memReserve(n)
}

// reserveScript: increments only when the result stays within the budget.
// Returns new count on success, or -(current+1) on refusal.
var reserveScript = redis.NewScript(`
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
local n = tonumber(ARGV[1]); local budget = tonumber(ARGV[2])
if cur + n > budget then return -(cur + 1) end
local v = redis.call('INCRBY', KEYS[1], n)
redis.call('EXPIRE', KEYS[1], tonumber(ARGV[3]))
return v
`)

func (c *Counter) memUsed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollMem()
	return c.memN
}

func (c *Counter) memReserve(n int) (bool, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rollMem()
	if c.memN+n > c.Budget {
		return false, c.memN
	}
	c.memN += n
	return true, c.memN
}

func (c *Counter) rollMem() {
	day := Key(c.Now())
	if day != c.memDay {
		c.memDay, c.memN = day, 0
	}
}

// Ping reports whether Redis is reachable (nil when Redis is not configured).
func (c *Counter) Ping(ctx context.Context) error {
	if c.rdb == nil {
		return nil
	}
	return c.rdb.Ping(ctx).Err()
}

func (c *Counter) Backend() string {
	if c.rdb != nil {
		return "redis"
	}
	return "memory"
}

func (c *Counter) String() string { return fmt.Sprintf("quota(budget=%d, %s)", c.Budget, c.Backend()) }
