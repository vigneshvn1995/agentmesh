// Package httputil provides lightweight HTTP response helpers shared across
// the AgentMesh middleware layers.
//
// WriteJSONError gives every error path a consistent JSON shape:
//
//	{"error": "<human message>", "reason": "<machine code>"}
//
// ResponseRecorder (recorder.go) wraps an http.ResponseWriter to capture the
// status code and response body so that post-flight middleware (budget
// recording, cache storing) can inspect what was sent to the client.
package httputil

import (
	"encoding/json"
	"net/http"
)

// errorBody is the canonical JSON shape for all agentmesh error responses.
type errorBody struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

// WriteJSONError writes a JSON-encoded error response to w.
// It sets Content-Type to application/json and the HTTP status to status.
// The response body is:
//
//	{"error":"<message>","reason":"<reason>"}
func WriteJSONError(w http.ResponseWriter, status int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	// json.NewEncoder writes directly to w; errors here are unreachable in
	// normal operation because w is always a valid io.Writer.
	_ = json.NewEncoder(w).Encode(errorBody{
		Error:  message,
		Reason: reason,
	})
}
