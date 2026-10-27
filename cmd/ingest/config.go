package main

import (
	"fmt"
	"os"
	"strconv"
)

const defaultPort = 8080

// listenAddr returns the HTTP listen address from INGEST_PORT, defaulting to :8080.
// An invalid port is an error so a typo fails at startup rather than at bind time.
func listenAddr() (string, error) {
	raw := os.Getenv("INGEST_PORT")
	if raw == "" {
		return fmt.Sprintf(":%d", defaultPort), nil
	}
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("INGEST_PORT must be a number from 1 to 65535, got %q", raw)
	}
	return fmt.Sprintf(":%d", port), nil
}
