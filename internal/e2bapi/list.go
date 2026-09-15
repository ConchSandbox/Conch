package e2bapi

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/openeuler/Conch/internal/sandbox"
)

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, 400, "invalid query")
		return
	}
	descending := true
	switch query.Get("order") {
	case "", "desc":
	case "asc":
		descending = false
	default:
		writeError(w, 400, "order must be asc or desc")
		return
	}
	includeRunning := true
	if state := query.Get("state"); state != "" {
		includeRunning = false
		for _, item := range strings.Split(state, ",") {
			switch item {
			case "running":
				includeRunning = true
			case "paused":
			default:
				writeError(w, 400, "state must be running or paused")
				return
			}
		}
	}
	metadata, err := url.ParseQuery(query.Get("metadata"))
	if err != nil {
		writeError(w, 400, "invalid metadata filter")
		return
	}
	var after time.Time
	if value := query.Get("startedAfter"); value != "" {
		after, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			writeError(w, 400, "invalid startedAfter timestamp")
			return
		}
	}
	// An absent limit must return the entire local set: AgentENV Gateway
	// removes limit/nextToken before fan-out and performs cluster pagination.
	limit := 0
	if value := query.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, 400, "limit must be between 1 and 100")
			return
		}
	}
	var cursorTime time.Time
	var cursorID string
	if token := query.Get("nextToken"); token != "" {
		cursorTime, cursorID, err = parseCursor(token, descending)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}
	records, err := s.runtime.ListSandboxes(r.Context())
	if err != nil {
		s.runtimeError(w, err)
		return
	}
	items := make([]sandboxDetail, 0, len(records))
	for _, rec := range records {
		if !includeRunning || !rec.E2B || rec.State != sandbox.StateReady {
			continue
		}
		if template := query.Get("template"); template != "" && template != rec.SourceTemplateID && template != rec.SourceTemplateName {
			continue
		}
		if !after.IsZero() && time.Unix(0, rec.CreatedAt).Before(after) {
			continue
		}
		matched := true
		for key, values := range metadata {
			value, ok := rec.Metadata[key]
			for _, expected := range values {
				if !ok || value != expected {
					matched = false
				}
			}
		}
		if matched {
			items = append(items, s.detail(rec))
		}
	}
	if includeRunning {
		w.Header().Set("X-Total-Running", strconv.Itoa(len(items)))
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].StartedAt.Equal(items[j].StartedAt) {
			if descending {
				return items[i].SandboxID < items[j].SandboxID
			}
			return items[i].SandboxID > items[j].SandboxID
		}
		if descending {
			return items[i].StartedAt.After(items[j].StartedAt)
		}
		return items[i].StartedAt.Before(items[j].StartedAt)
	})
	page := make([]sandboxDetail, 0, len(items))
	for _, item := range items {
		if !cursorTime.IsZero() {
			past := item.StartedAt.Before(cursorTime) || (item.StartedAt.Equal(cursorTime) && item.SandboxID > cursorID)
			if !descending {
				past = item.StartedAt.After(cursorTime) || (item.StartedAt.Equal(cursorTime) && item.SandboxID < cursorID)
			}
			if !past {
				continue
			}
		}
		page = append(page, item)
	}
	if limit > 0 && len(page) >= limit {
		page = page[:limit]
		last := page[len(page)-1]
		raw := last.StartedAt.Format(time.RFC3339Nano) + "__" + last.SandboxID
		if !descending {
			raw += "__asc"
		}
		w.Header().Set("X-Next-Token", base64.URLEncoding.EncodeToString([]byte(raw)))
	}
	writeJSON(w, 200, page)
}

func parseCursor(token string, descending bool) (time.Time, string, error) {
	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid nextToken")
	}
	parts := strings.Split(string(decoded), "__")
	if len(parts) != 2 && len(parts) != 3 {
		return time.Time{}, "", fmt.Errorf("invalid nextToken")
	}
	if (len(parts) == 2) != descending || (len(parts) == 3 && parts[2] != "asc") {
		return time.Time{}, "", fmt.Errorf("nextToken order does not match request")
	}
	stamp, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid nextToken timestamp")
	}
	parsed, err := uuid.Parse(parts[1])
	if err != nil || parsed.String() != parts[1] {
		return time.Time{}, "", fmt.Errorf("invalid nextToken sandbox ID")
	}
	return stamp, parts[1], nil
}
