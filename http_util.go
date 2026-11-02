package main

import (
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		slog.Error("could not write JSON response", "error", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func isUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for _, position := range []int{8, 13, 18, 23} {
		if value[position] != '-' {
			return false
		}
	}
	_, err := hex.DecodeString(strings.ReplaceAll(value, "-", ""))
	return err == nil
}
