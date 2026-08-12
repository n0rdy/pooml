package common

const (
	// envs:
	LocalEnv = "local"
	ProEnv   = "pro"

	// log levels:
	LevelTrace = 0
	LevelDebug = 1
	LevelInfo  = 2
	LevelWarn  = 3
	LevelError = 4
	LevelFatal = 5
)

var (
	SupportedEnvs = map[string]bool{
		LocalEnv: true,
		ProEnv:   true,
	}
)
