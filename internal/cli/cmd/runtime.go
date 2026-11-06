package cmd

import (
	"fmt"
	"strings"
	"time"
)

func ResolveConchAPIURL(apiURLOverride, addressAlias string) string {
	if apiURLOverride != "" {
		return apiURLOverride
	}
	return addressAlias
}

func ParseRegistryUser(user string) (string, string, error) {
	if user == "" {
		return "", "", nil
	}
	idx := strings.IndexByte(user, ':')
	if idx <= 0 || idx == len(user)-1 {
		return "", "", fmt.Errorf("invalid --user value %q, want username:password", user)
	}
	return user[:idx], user[idx+1:], nil
}

func parseTimeoutFlag(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 {
		return 0, fmt.Errorf("invalid --timeout %q", raw)
	}
	return timeout, nil
}
