package common

// LoginPageData contains data for the login page
type LoginPageData struct {
	Title string
	Error string
}

// StandardLog is the normalized form every parser produces; maps 1:1 to the
// logs table. Nil pointers become NULL columns.
type StandardLog struct {
	Timestamp  int64
	IngestedAt int64
	Level      *int
	Service    string
	Host       string
	Message    *string
	Parsed     *string
	Raw        string
}

// ClampTimestamp guards against absurd client-supplied event times: a
// far-future timestamp pins itself above every "newest first" view and is
// never retention-deleted; a far-past one (epoch-zero bugs) is dead weight.
// The past bound matches the retention MAXIMUM (3650 days), so any backfill
// a user could legitimately retain survives. Outside the window the honest
// arrival time wins.
func ClampTimestamp(ts, receivedAt int64) int64 {
	const (
		maxFutureMs = int64(24 * 60 * 60 * 1000)
		maxPastMs   = int64(3650 * 24 * 60 * 60 * 1000)
	)
	if ts > receivedAt+maxFutureMs || ts < receivedAt-maxPastMs {
		return receivedAt
	}
	return ts
}
