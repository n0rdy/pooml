package common

type ErrorResponse struct {
	Code string `json:"code,omitempty"`
	// Message carries actionable detail where the code alone is useless -
	// e.g. the SQL validation error a query-API consumer must fix.
	Message string `json:"message,omitempty"`
}
