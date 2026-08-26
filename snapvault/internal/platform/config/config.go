// Package config reads service configuration from environment variables.
package config

import (
	"os"
	"strconv"
	"strings"
)

// String returns the value of key, or def if it is unset or empty.
func String(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// MustString returns the value of key, or panics if it is unset or empty.
// Use it for configuration a service cannot run without.
func MustString(key string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	panic("config: required environment variable " + key + " is not set")
}

// Int returns the integer value of key, or def if it is unset or invalid.
func Int(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// FileOrEnv returns the contents of the file named by key+"_FILE" if that
// variable is set, otherwise the value of key. This lets the same binary read
// a secret mounted as a file in the cluster (via the Secret Manager CSI
// driver) and a plain environment variable on a developer's laptop.
func FileOrEnv(key string) string {
	if path := os.Getenv(key + "_FILE"); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return os.Getenv(key)
}
