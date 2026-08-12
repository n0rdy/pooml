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
