package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyw14/cry-98/internal/catalog"
	"github.com/wyw14/cry-98/internal/circuit"
	"github.com/wyw14/cry-98/internal/probe"
	"github.com/wyw14/cry-98/internal/vessel"
)

func newCatalogServer(t *testing.T) (*Server, *catalog.Service) {
	t.Helper()
	vessels := vessel.NewRegistry()
	probes := probe.NewRegistry(8)
	circuits := circuit.NewRegistry()
	service := catalog.NewService(
		catalog.NewFileStore(filepath.Join(t.TempDir(), "devices.json")),
		vessels, probes, circuits, nil, catalog.Hooks{},
		func() time.Time { return time.Unix(100, 0) },
	)
	if err := service.Load(); err != nil {
		t.Fatal(err)
	}
	server := NewServer(Dependencies{Catalog: service})
	return server, service
}

func doJSON(t *testing.T, handler http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	result := map[string]any{}
	if response.Body.Len() > 0 {
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatalf("invalid json response: %v (%q)", err, response.Body.String())
		}
	}
	return response.Code, result
}

func TestDeviceCatalogEndToEnd(t *testing.T) {
	server, _ := newCatalogServer(t)
	handler := server.Handler()

	status, body := doJSON(t, handler, http.MethodGet, "/api/devices", nil)
	if status != http.StatusOK {
		t.Fatalf("list status=%d", status)
	}
	items, _ := body["items"].([]any)
	if len(items) != 13 {
		t.Fatalf("devices=%d want 13", len(items))
	}

	// Register a new vessel.
	status, body = doJSON(t, handler, http.MethodPost, "/api/devices", map[string]any{
		"id": "V-99", "type": "vessel", "name": "New", "zone": "west",
	})
	if status != http.StatusCreated {
		t.Fatalf("register status=%d body=%v", status, body)
	}

	// Register a probe bound to the new vessel.
	status, body = doJSON(t, handler, http.MethodPost, "/api/devices", map[string]any{
		"id": "P-99", "type": "probe", "probe_kind": "level", "vessel_id": "V-99",
	})
	if status != http.StatusCreated {
		t.Fatalf("probe register status=%d body=%v", status, body)
	}

	// A probe bound to an unknown vessel is rejected with 400.
	status, _ = doJSON(t, handler, http.MethodPost, "/api/devices", map[string]any{
		"id": "P-BAD", "type": "probe", "probe_kind": "level", "vessel_id": "V-404",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid register status=%d", status)
	}

	// Duplicate id rejected with 400.
	status, _ = doJSON(t, handler, http.MethodPost, "/api/devices", map[string]any{
		"id": "C-A", "type": "vessel", "name": "Dup", "zone": "north",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("duplicate register status=%d", status)
	}

	// Modify the vessel.
	status, body = doJSON(t, handler, http.MethodPut, "/api/devices/V-99", map[string]any{
		"name": "Renamed",
	})
	if status != http.StatusOK || body["name"] != "Renamed" {
		t.Fatalf("update status=%d body=%v", status, body)
	}

	// Rebind the probe to an existing vessel.
	status, body = doJSON(t, handler, http.MethodPut, "/api/devices/P-99", map[string]any{
		"vessel_id": "V-03",
	})
	if status != http.StatusOK || body["vessel_id"] != "V-03" {
		t.Fatalf("rebind status=%d body=%v", status, body)
	}

	// Deactivating V-99 now works (its probe moved away).
	status, body = doJSON(t, handler, http.MethodPost, "/api/devices/V-99/deactivate", nil)
	if status != http.StatusOK || body["status"] != "deactivated" {
		t.Fatalf("deactivate status=%d body=%v", status, body)
	}

	// V-03 still has an active bound probe (P-99 and P-L03); deactivating
	// it directly must fail because the binding would dangle.
	status, _ = doJSON(t, handler, http.MethodPost, "/api/devices/V-03/deactivate", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("deactivate vessel with probe status=%d", status)
	}

	// Unknown device returns 404.
	status, _ = doJSON(t, handler, http.MethodPost, "/api/devices/V-NONE/deactivate", nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing device status=%d", status)
	}
}
