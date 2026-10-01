// Package workers runs background maintenance loops such as the persistent
// reminder scheduler.
package workers

import (
	"context"
	"time"

	"openrouter-bot/features/reminders"
)

// DeliverFunc sends a triggered reminder to its target chat.
type DeliverFunc func(r reminders.Reminder)

// StartReminderScheduler runs a background ticker that checks for due
// reminders on startup (so reminders missed during a restart fire immediately)
// and every interval thereafter until ctx is cancelled.
func StartReminderScheduler(
	ctx context.Context,
	mgr *reminders.Manager,
	interval time.Duration,
	deliver DeliverFunc,
) {
	if mgr == nil || deliver == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}

	dispatch := func() {
		due := mgr.PopDue(time.Now())
		for _, r := range due {
			deliver(r)
		}
	}

	go func() {
		// Fire any overdue reminders immediately on startup.
		dispatch()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				dispatch()
			}
		}
	}()
}
