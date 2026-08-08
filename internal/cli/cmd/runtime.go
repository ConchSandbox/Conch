package cmd

import (
	"fmt"
	"strings"
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
