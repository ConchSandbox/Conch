package e2bapi

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type snapshotModel struct {
	SnapshotID string   `json:"snapshotID"`
	Names      []string `json:"names"`
	CPUCount   int64    `json:"cpuCount"`
	MemoryMB   int64    `json:"memoryMB"`
	CreatedAt  string   `json:"createdAt"`
	UpdatedAt  string   `json:"updatedAt"`
}

// listSnapshots implements GET /snapshots: the E2B snapshot list, newest
// first. It enumerates the checkpoint templates captured through the snapshots
// API; pause checkpoints are internal and are not listed.
func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, 400, "invalid query")
		return
	}
	// An absent limit must return the entire local set, like the sandbox list.
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
		cursorTime, cursorID, err = parseSnapshotCursor(token)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}
	snapshots, err := s.runtime.ListSandboxSnapshots(r.Context(), query.Get("sandboxID"), query.Get("name"))
	if err != nil {
		s.runtimeError(r.Context(), w, err)
		return
	}
	page := make([]snapshotModel, 0, len(snapshots))
	for _, snapshot := range snapshots {
		created := time.Unix(0, snapshot.CreatedAt).UTC()
		if !cursorTime.IsZero() {
			past := created.Before(cursorTime) || (created.Equal(cursorTime) && snapshot.SnapshotID > cursorID)
			if !past {
				continue
			}
		}
		stamp := created.Format(time.RFC3339Nano)
		page = append(page, snapshotModel{
			SnapshotID: snapshot.SnapshotID, Names: snapshot.Names, CPUCount: snapshot.CPUCount,
			MemoryMB: snapshot.MemoryMB, CreatedAt: stamp, UpdatedAt: stamp,
		})
	}
	if limit > 0 && len(page) > limit {
		page = page[:limit]
		last := page[len(page)-1]
		raw := last.CreatedAt + "__" + last.SnapshotID
		w.Header().Set("X-Next-Token", base64.URLEncoding.EncodeToString([]byte(raw)))
	}
	writeJSON(w, 200, page)
}

func parseSnapshotCursor(token string) (time.Time, string, error) {
	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid nextToken")
	}
	stamp, name, ok := strings.Cut(string(decoded), "__")
	if !ok {
		return time.Time{}, "", fmt.Errorf("invalid nextToken")
	}
	parsed, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("invalid nextToken timestamp")
	}
	if name == "" {
		return time.Time{}, "", fmt.Errorf("invalid nextToken snapshot ID")
	}
	return parsed, name, nil
}
