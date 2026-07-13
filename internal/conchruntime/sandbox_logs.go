package conchruntime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	defaultSandboxLogLimit = 1024
	defaultSandboxLogTTL   = time.Hour
)

type SandboxLogBuffer struct {
	mu        sync.Mutex
	limit     int
	ttl       time.Duration
	now       func() time.Time
	nextID    map[string]uint64
	aliases   map[string]string
	expiresAt map[string]time.Time
	entries   map[string][]SandboxLogEntry
}

func newSandboxLogBuffer(limit int, ttl ...time.Duration) *SandboxLogBuffer {
	if limit <= 0 {
		limit = defaultSandboxLogLimit
	}
	logTTL := defaultSandboxLogTTL
	if len(ttl) > 0 && ttl[0] > 0 {
		logTTL = ttl[0]
	}
	return &SandboxLogBuffer{
		limit:     limit,
		ttl:       logTTL,
		now:       time.Now,
		nextID:    make(map[string]uint64),
		aliases:   make(map[string]string),
		expiresAt: make(map[string]time.Time),
		entries:   make(map[string][]SandboxLogEntry),
	}
}

func (b *SandboxLogBuffer) Append(sandboxID, level, message string) {
	sandboxID = strings.TrimSpace(sandboxID)
	level = strings.ToLower(strings.TrimSpace(level))
	message = strings.TrimSpace(message)
	if b == nil || sandboxID == "" || message == "" {
		return
	}
	if level == "" {
		level = "info"
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now().UTC()
	b.pruneExpiredLocked(now)
	delete(b.expiresAt, sandboxID)
	b.nextID[sandboxID]++
	logs := append(b.entries[sandboxID], SandboxLogEntry{
		ID:        b.nextID[sandboxID],
		Time:      now,
		SandboxID: sandboxID,
		Level:     level,
		Message:   message,
	})
	if len(logs) > b.limit {
		logs = logs[len(logs)-b.limit:]
	}
	b.entries[sandboxID] = logs
}

func (b *SandboxLogBuffer) Get(opts SandboxLogsOptions) SandboxLogsResult {
	opts.SandboxID = strings.TrimSpace(opts.SandboxID)
	if b == nil || opts.SandboxID == "" {
		return SandboxLogsResult{Logs: []SandboxLogEntry{}, NextCursor: opts.Cursor}
	}
	level := strings.ToLower(strings.TrimSpace(opts.Level))
	search := strings.ToLower(strings.TrimSpace(opts.Search))
	limit := opts.Limit
	out := make([]SandboxLogEntry, 0)
	nextCursor := opts.Cursor

	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneExpiredLocked(b.now().UTC())
	sandboxID := b.resolveLogIDLocked(opts.SandboxID)
	for _, entry := range b.entries[sandboxID] {
		if entry.ID <= opts.Cursor {
			continue
		}
		nextCursor = entry.ID
		if level != "" && strings.ToLower(entry.Level) != level {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(entry.Message), search) {
			continue
		}
		out = append(out, entry)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	if out == nil {
		out = []SandboxLogEntry{}
	}
	return SandboxLogsResult{Logs: out, NextCursor: nextCursor}
}

func (b *SandboxLogBuffer) Expire(sandboxID string) {
	sandboxID = strings.TrimSpace(sandboxID)
	if b == nil || sandboxID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.entries[sandboxID]; !ok {
		return
	}
	b.expiresAt[sandboxID] = b.now().UTC().Add(b.ttl)
}

func (b *SandboxLogBuffer) Alias(podSandboxID, conchSandboxID string) {
	podSandboxID = strings.TrimSpace(podSandboxID)
	conchSandboxID = strings.TrimSpace(conchSandboxID)
	if b == nil || podSandboxID == "" || conchSandboxID == "" || podSandboxID == conchSandboxID {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.aliases[podSandboxID] = conchSandboxID
}

func (b *SandboxLogBuffer) Resolve(sandboxID string) string {
	sandboxID = strings.TrimSpace(sandboxID)
	if b == nil || sandboxID == "" {
		return sandboxID
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneExpiredLocked(b.now().UTC())
	return b.resolveLogIDLocked(sandboxID)
}

func (b *SandboxLogBuffer) resolveLogIDLocked(sandboxID string) string {
	if resolved := strings.TrimSpace(b.aliases[sandboxID]); resolved != "" {
		return resolved
	}
	return sandboxID
}

func (b *SandboxLogBuffer) pruneExpiredLocked(now time.Time) {
	for sandboxID, expiresAt := range b.expiresAt {
		if now.Before(expiresAt) {
			continue
		}
		delete(b.entries, sandboxID)
		delete(b.nextID, sandboxID)
		delete(b.expiresAt, sandboxID)
		b.deleteAliasesForLocked(sandboxID)
	}
}

func (b *SandboxLogBuffer) deleteAliasesForLocked(conchSandboxID string) {
	for alias, target := range b.aliases {
		if target == conchSandboxID || alias == conchSandboxID {
			delete(b.aliases, alias)
		}
	}
}

func (b *SandboxLogBuffer) Clear(sandboxID string) {
	sandboxID = strings.TrimSpace(sandboxID)
	if b == nil || sandboxID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.entries, sandboxID)
	delete(b.nextID, sandboxID)
	delete(b.expiresAt, sandboxID)
	b.deleteAliasesForLocked(sandboxID)
}

func (s *Service) AppendSandboxLog(sandboxID, level, message string) {
	if s == nil {
		return
	}
	if s.SandboxLogs == nil {
		s.SandboxLogs = newSandboxLogBuffer(defaultSandboxLogLimit)
	}
	s.SandboxLogs.Append(sandboxID, level, message)
}

func (s *Service) SetSandboxLogTTL(ttl time.Duration) {
	if s == nil || ttl <= 0 {
		return
	}
	if s.SandboxLogs == nil {
		s.SandboxLogs = newSandboxLogBuffer(defaultSandboxLogLimit, ttl)
		return
	}
	s.SandboxLogs.mu.Lock()
	defer s.SandboxLogs.mu.Unlock()
	s.SandboxLogs.ttl = ttl
}

func (s *Service) ExpireSandboxLogs(sandboxID string) {
	if s == nil || s.SandboxLogs == nil {
		return
	}
	s.SandboxLogs.Expire(sandboxID)
}

func (s *Service) AliasSandboxLogs(podSandboxID, conchSandboxID string) {
	if s == nil {
		return
	}
	if s.SandboxLogs == nil {
		s.SandboxLogs = newSandboxLogBuffer(defaultSandboxLogLimit)
	}
	s.SandboxLogs.Alias(podSandboxID, conchSandboxID)
}

func (s *Service) ClearSandboxLogs(sandboxID string) {
	if s == nil || s.SandboxLogs == nil {
		return
	}
	s.SandboxLogs.Clear(sandboxID)
}

func (s *Service) GetSandboxLogs(ctx context.Context, opts SandboxLogsOptions) (SandboxLogsResult, error) {
	if strings.TrimSpace(opts.SandboxID) == "" {
		return SandboxLogsResult{}, fmt.Errorf("sandbox id is required")
	}
	if s == nil || s.SandboxLogs == nil {
		return SandboxLogsResult{Logs: []SandboxLogEntry{}, NextCursor: opts.Cursor}, nil
	}
	opts.SandboxID = s.resolveSandboxLogID(ctx, opts.SandboxID)
	return s.SandboxLogs.Get(opts), nil
}

func (s *Service) resolveSandboxLogID(ctx context.Context, sandboxID string) string {
	sandboxID = strings.TrimSpace(sandboxID)
	if s == nil || sandboxID == "" {
		return sandboxID
	}
	if s.SandboxLogs != nil {
		if resolved := s.SandboxLogs.Resolve(sandboxID); resolved != sandboxID {
			return resolved
		}
	}
	if s.Store == nil {
		return sandboxID
	}
	rec, err := s.getSandbox(ctx, sandboxID)
	if err != nil || strings.TrimSpace(rec.ConchSandboxID) == "" {
		return sandboxID
	}
	return strings.TrimSpace(rec.ConchSandboxID)
}
