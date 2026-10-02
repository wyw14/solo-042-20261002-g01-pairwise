package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-98/internal/catalog"
	"github.com/wyw14/cry-98/internal/model"
)

type registerVesselRequest struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Zone string `json:"zone"`
}

type modifyVesselRequest struct {
	Name string `json:"name"`
	Zone string `json:"zone"`
}

type registerProbeRequest struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	VesselID string `json:"vessel_id"`
}

type modifyProbeRequest struct {
	Kind     string `json:"kind"`
	VesselID string `json:"vessel_id"`
}

type registerCircuitRequest struct {
	ID      string `json:"id"`
	Zone    string `json:"zone"`
	ValveID string `json:"valve_id"`
}

type modifyCircuitRequest struct {
	Zone    string `json:"zone"`
	ValveID string `json:"valve_id"`
}

func (s *Server) getCatalog(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, s.deps.Catalog.View())
}

func (s *Server) registerVessel(writer http.ResponseWriter, request *http.Request) {
	var input registerVesselRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	record, err := s.deps.Catalog.RegisterVessel(input.ID, input.Name, input.Zone)
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, record)
}

func (s *Server) modifyVessel(writer http.ResponseWriter, request *http.Request) {
	var input modifyVesselRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	record, err := s.deps.Catalog.ModifyVessel(chi.URLParam(request, "id"), input.Name, input.Zone)
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, record)
}

func (s *Server) deactivateVessel(writer http.ResponseWriter, request *http.Request) {
	record, err := s.deps.Catalog.DeactivateVessel(chi.URLParam(request, "id"))
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, record)
}

func (s *Server) registerProbe(writer http.ResponseWriter, request *http.Request) {
	var input registerProbeRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	record, err := s.deps.Catalog.RegisterProbe(input.ID, model.ProbeKind(input.Kind), input.VesselID)
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, record)
}

func (s *Server) modifyProbe(writer http.ResponseWriter, request *http.Request) {
	var input modifyProbeRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	record, err := s.deps.Catalog.ModifyProbe(chi.URLParam(request, "id"), model.ProbeKind(input.Kind), input.VesselID)
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, record)
}

func (s *Server) deactivateProbe(writer http.ResponseWriter, request *http.Request) {
	record, err := s.deps.Catalog.DeactivateProbe(chi.URLParam(request, "id"))
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, record)
}

func (s *Server) registerCircuit(writer http.ResponseWriter, request *http.Request) {
	var input registerCircuitRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	record, err := s.deps.Catalog.RegisterCircuit(input.ID, input.Zone, input.ValveID)
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, record)
}

func (s *Server) modifyCircuit(writer http.ResponseWriter, request *http.Request) {
	var input modifyCircuitRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	record, err := s.deps.Catalog.ModifyCircuit(chi.URLParam(request, "id"), input.Zone, input.ValveID)
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, record)
}

func (s *Server) deactivateCircuit(writer http.ResponseWriter, request *http.Request) {
	record, err := s.deps.Catalog.DeactivateCircuit(chi.URLParam(request, "id"))
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, record)
}

func writeCatalogError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeError(writer, http.StatusNotFound, err.Error())
	case errors.Is(err, catalog.ErrDuplicate),
		errors.Is(err, catalog.ErrInactive),
		errors.Is(err, catalog.ErrOccupied),
		errors.Is(err, catalog.ErrBound):
		writeError(writer, http.StatusConflict, err.Error())
	default:
		writeError(writer, http.StatusBadRequest, err.Error())
	}
}
