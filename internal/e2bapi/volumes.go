package e2bapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/openeuler/Conch/internal/runtimeapi"
	"github.com/openeuler/Conch/internal/volume"
)

type volumeCreateRequest struct {
	Name string `json:"name"`
}

type volumeModel struct {
	VolumeID string `json:"volumeID"`
	Name     string `json:"name"`
}

type volumeCreateModel struct {
	VolumeID string `json:"volumeID"`
	Name     string `json:"name"`
	Token    string `json:"token"`
}

// volumes routes the E2B volume registry API. Volumes are node-local: the
// registry and data directories live on this node only.
func (s *Server) volumeRoutes(w http.ResponseWriter, r *http.Request, parts []string) {
	if s.volumes == nil {
		unimplemented(w, "volume API is not configured on this node")
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodPost:
		s.createVolume(w, r)
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.listVolumes(w, r)
	case len(parts) == 2 && r.Method == http.MethodGet:
		s.getVolume(w, r, parts[1])
	case len(parts) == 2 && r.Method == http.MethodDelete:
		s.deleteVolume(w, r, parts[1])
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) createVolume(w http.ResponseWriter, r *http.Request) {
	var request volumeCreateRequest
	if !decodeBody(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	// A valid digest would be ambiguous for the create endpoint, which
	// resolves volume names and template IDs by the same rule.
	if _, err := uuid.Parse(request.Name); err == nil {
		writeError(w, http.StatusBadRequest, "name must not be a UUID")
		return
	}
	record, err := s.volumes.Create(r.Context(), request.Name)
	if err != nil {
		s.runtimeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, volumeCreateModel{
		VolumeID: record.ID, Name: record.Name, Token: record.Token,
	})
}

func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request) {
	records, err := s.volumes.List(r.Context())
	if err != nil {
		s.runtimeError(w, err)
		return
	}
	volumes := make([]volumeModel, 0, len(records))
	for _, record := range records {
		volumes = append(volumes, volumeModel{VolumeID: record.ID, Name: record.Name})
	}
	writeJSON(w, http.StatusOK, volumes)
}

func (s *Server) getVolume(w http.ResponseWriter, r *http.Request, id string) {
	record, err := s.volumes.Get(r.Context(), id)
	if err != nil {
		s.runtimeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, volumeModel{VolumeID: record.ID, Name: record.Name})
}

func (s *Server) deleteVolume(w http.ResponseWriter, r *http.Request, id string) {
	inUse := func(volumeID string) bool {
		records, err := s.runtime.ListSandboxes(context.WithoutCancel(r.Context()))
		if err != nil {
			// Fail closed: an unreadable store must not free mounted data.
			return true
		}
		for _, rec := range records {
			for _, mount := range rec.VolumeMounts {
				if mount.Name == "" {
					continue
				}
				// Volume mounts are resolved by name; keep the check cheap
				// by comparing against the requested volume's record.
				if matched, _ := s.volumeMountMatches(r.Context(), volumeID, mount.Name); matched {
					return true
				}
			}
		}
		return false
	}
	if err := s.volumes.Delete(r.Context(), id, inUse); err != nil {
		s.runtimeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// volumeMountMatches reports whether the named mount on any sandbox resolves
// to the given volume ID.
func (s *Server) volumeMountMatches(ctx context.Context, volumeID, name string) (bool, error) {
	record, err := s.volumes.Get(ctx, volumeID)
	if err != nil {
		return false, err
	}
	return record.Name == name, nil
}

// volumeAPI is the subset of volume.Registry the E2B facade needs: the volume
// registry CRUD plus named-mount resolution for create requests.
type volumeAPI interface {
	Create(ctx context.Context, name string) (volume.VolumeRecord, error)
	Get(ctx context.Context, id string) (volume.VolumeRecord, error)
	List(ctx context.Context) ([]volume.VolumeRecord, error)
	Delete(ctx context.Context, id string, inUse func(volumeID string) bool) error
	ResolveMounts(ctx context.Context, mounts []runtimeapi.VolumeMountSpec) ([]runtimeapi.VolumeMountSpec, error)
}
