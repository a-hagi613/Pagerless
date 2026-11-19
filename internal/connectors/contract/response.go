package contract

import "net/http"

// Response is one bounded backend response.
type Response struct {
	// Status is the HTTP status.
	Status int
	// Header is the response header.
	Header http.Header
	// Body is the response body, capped at the Connector's byte ceiling.
	Body []byte
	// Truncated reports whether the cap cut the body.
	Truncated bool
}
