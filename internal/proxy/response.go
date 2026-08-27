package proxy

import (
	"encoding/json"
	"net/http"
)

// errorEnvelope matches the OpenAI error shape so existing clients can parse
// gateway errors without special-casing them.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line is already sent, so a failed write can only be logged by
	// the caller's middleware; there is nothing useful to do here.
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, kind, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Message: message, Type: kind}})
}
