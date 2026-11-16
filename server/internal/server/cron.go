package server

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a standard 5-field cron expression (minute hour day-of-month
// month day-of-week), evaluated in UTC like Nakama's leaderboard resets.
type Cron struct {
	min, hour, dom, month, dow [64]bool
	domAny, dowAny             bool
}

func parseField(s string, lo, hi int, out *[64]bool) (bool, error) {
	any := false
	for _, part := range strings.Split(s, ",") {
		step := 1
		if i := strings.IndexByte(part, '/'); i >= 0 {
			n, err := strconv.Atoi(part[i+1:])
			if err != nil || n <= 0 {
				return false, fmt.Errorf("bad step %q", part)
			}
			step, part = n, part[:i]
		}
		a, b := lo, hi
		switch {
		case part == "*":
			if step == 1 {
				any = true
			}
		case strings.Contains(part, "-"):
			x := strings.SplitN(part, "-", 2)
			var err1, err2 error
			a, err1 = strconv.Atoi(x[0])
			b, err2 = strconv.Atoi(x[1])
			if err1 != nil || err2 != nil {
				return false, fmt.Errorf("bad range %q", part)
			}
		default:
			n, err := strconv.Atoi(part)
			if err != nil {
				return false, fmt.Errorf("bad value %q", part)
			}
			a, b = n, n
			if step > 1 {
				b = hi
			}
		}
		if a < lo || b > hi || a > b {
			return false, fmt.Errorf("value out of range in %q", s)
		}
		for v := a; v <= b; v += step {
			out[v] = true
		}
	}
	return any, nil
}

func ParseCron(expr string) (*Cron, error) {
	f := strings.Fields(expr)
	if len(f) != 5 {
		return nil, fmt.Errorf("cron expression %q must have 5 fields", expr)
	}
	c := &Cron{}
	var err error
	if _, err = parseField(f[0], 0, 59, &c.min); err != nil {
		return nil, err
	}
	if _, err = parseField(f[1], 0, 23, &c.hour); err != nil {
		return nil, err
	}
	if c.domAny, err = parseField(f[2], 1, 31, &c.dom); err != nil {
		return nil, err
	}
	if _, err = parseField(f[3], 1, 12, &c.month); err != nil {
		return nil, err
	}
	if c.dowAny, err = parseField(f[4], 0, 7, &c.dow); err != nil {
		return nil, err
	}
	if c.dow[7] {
		c.dow[0] = true
	}
	return c, nil
}

func (c *Cron) dayMatches(t time.Time) bool {
	domOK := c.dom[t.Day()]
	dowOK := c.dow[int(t.Weekday())]
	switch {
	case c.domAny && c.dowAny:
		return true
	case c.domAny:
		return dowOK
	case c.dowAny:
		return domOK
	default:
		return domOK || dowOK
	}
}

// Next returns the first matching minute strictly after t.
func (c *Cron) Next(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if !c.month[int(t.Month())] {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !c.dayMatches(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if !c.hour[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, time.UTC)
			continue
		}
		if !c.min[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}
