package ui

import "time"

// SetStreamIntervalsForTest shrinks the live-tail timers so stream tests run
// in milliseconds; returns a restore func.
func SetStreamIntervalsForTest(poll, refresh time.Duration) (restore func()) {
	oldPoll, oldRefresh := streamPollInterval, streamRefreshInterval
	streamPollInterval, streamRefreshInterval = poll, refresh
	return func() {
		streamPollInterval, streamRefreshInterval = oldPoll, oldRefresh
	}
}
