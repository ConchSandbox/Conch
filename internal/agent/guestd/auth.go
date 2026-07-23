package guestd

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"sync"
)

const agentTokenHeaderKey = "conch-init-token"

var agentAuth = &agentTokenAuth{}

type agentTokenAuth struct {
	mu    sync.RWMutex
	token string
}

func (a *agentTokenAuth) SetToken(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.token = strings.TrimSpace(token)
}

func (a *agentTokenAuth) verifyHTTPHeader(header http.Header) error {
	a.mu.RLock()
	expected := a.token
	a.mu.RUnlock()
	if expected == "" {
		return errors.New("agent token is not initialized")
	}

	values := header.Values(agentTokenHeaderKey)
	if len(values) == 0 || values[0] == "" {
		return errors.New("agent token is required")
	}
	got := values[0]
	if len(got) > 4096 {
		return errors.New("agent token is invalid")
	}
	if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
		return errors.New("agent token is invalid")
	}
	return nil
}
