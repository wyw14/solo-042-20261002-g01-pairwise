package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wyw14/cry-98/internal/catalog"
)

func (s *Server) listDevices(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"items": s.deps.Catalog.List()})
}

func (s *Server) registerDevice(writer http.ResponseWriter, request *http.Request) {
	var input catalog.DeviceRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	device, err := s.deps.Catalog.Register(input)
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, device)
}

func (s *Server) updateDevice(writer http.ResponseWriter, request *http.Request) {
	id := chi.URLParam(request, "id")
	var input catalog.DeviceRequest
	if err := decodeJSON(request, &input); err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	device, err := s.deps.Catalog.Update(id, input)
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, device)
}

func (s *Server) deactivateDevice(writer http.ResponseWriter, request *http.Request) {
	id := chi.URLParam(request, "id")
	device, err := s.deps.Catalog.Deactivate(id)
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, device)
}

func writeCatalogError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrDeviceNotFound):
		writeError(writer, http.StatusNotFound, err.Error())
	case errors.Is(err, catalog.ErrDeviceOccupied):
		writeError(writer, http.StatusConflict, err.Error())
	default:
		writeError(writer, http.StatusBadRequest, err.Error())
	}
}
