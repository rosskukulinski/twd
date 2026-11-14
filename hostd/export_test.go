package hostd

import (
	"sync"
	"time"
)

// CronTickAt runs the cron scheduler's tick for the minute containing now
// (as cronLoop does at each minute boundary) and waits for the runs it
// starts to finish.
func (h *Host) CronTickAt(now time.Time) int {
	var wg sync.WaitGroup
	due := h.cronDue(now)
	for _, r := range due {
		wg.Add(1)
		go func() { defer wg.Done(); h.runScheduled(r) }()
	}
	wg.Wait()
	return len(due)
}
