package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyw14/cry-98/internal/circuit"
	"github.com/wyw14/cry-98/internal/fill"
	"github.com/wyw14/cry-98/internal/journal"
	"github.com/wyw14/cry-98/internal/model"
	"github.com/wyw14/cry-98/internal/probe"
	"github.com/wyw14/cry-98/internal/valve"
	"github.com/wyw14/cry-98/internal/vessel"
)

type rig struct {
	service  *Service
	store    *journal.Store
	vessels  *vessel.Registry
	probes   *probe.Registry
	circuits *circuit.Registry
	manager  *circuit.Manager
	fills    *fill.Coordinator
}

func newRig(t *testing.T, dir string, seed func(*Service) error) *rig {
	t.Helper()
	now := func() time.Time { return time.Unix(1_700_000_000, 0) }
	store, err := journal.NewStore(filepath.Join(dir, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	vessels := vessel.NewRegistry()
	probes := probe.NewRegistry(8)
	circuits := circuit.NewRegistry()
	manager := circuit.NewManager()
	transport := valve.NewMemoryTransport(now)
	controller, err := valve.NewController(transport, now)
	if err != nil {
		t.Fatal(err)
	}
	routes := valve.NewRouteService(valve.NewCommandService(controller, now))
	fills := fill.NewCoordinator(manager, circuits, routes, vessel.NewStateService(vessels, now), now)
	service, err := NewService(Dependencies{
		Vessels:  vessels,
		Probes:   probes,
		Circuits: circuits,
		Manager:  manager,
		Fills:    fills,
		Journal:  store,
		Seed:     seed,
		Now:      now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{
		service:  service,
		store:    store,
		vessels:  vessels,
		probes:   probes,
		circuits: circuits,
		manager:  manager,
		fills:    fills,
	}
}

func (r *rig) registerAll(t *testing.T) {
	t.Helper()
	if _, err := r.service.RegisterVessel("V-1", "Tank 1", "north"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.service.RegisterVessel("V-2", "Tank 2", "south"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.service.RegisterProbe("P-1", model.ProbeLevel, "V-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.service.RegisterCircuit("C-1", "north", "VALVE-1"); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) startFill(t *testing.T, vesselID string) {
	t.Helper()
	if _, err := r.fills.Start(context.Background(), vesselID, model.FillManual, 1.0); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSeedsEmptyCatalogOnce(t *testing.T) {
	dir := t.TempDir()
	seed := func(service *Service) error {
		if _, err := service.RegisterVessel("V-1", "Tank 1", "north"); err != nil {
			return err
		}
		return nil
	}
	first := newRig(t, dir, seed)
	if err := first.service.Load(); err != nil {
		t.Fatal(err)
	}
	if _, ok := first.vessels.Get("V-1"); !ok {
		t.Fatal("seeded vessel missing from runtime registry")
	}
	// A restart must reload the seeded catalog and must not seed again.
	second := newRig(t, dir, func(service *Service) error {
		t.Fatal("seed callback ran against a non-empty catalog")
		return nil
	})
	if err := second.service.Load(); err != nil {
		t.Fatal(err)
	}
	if got := second.service.View().Vessels; len(got) != 1 || got[0].ID != "V-1" || !got[0].Active {
		t.Fatalf("view=%+v", got)
	}
}

func TestRegisterPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	first := newRig(t, dir, nil)
	first.registerAll(t)
	if _, err := first.service.DeactivateCircuit("C-1"); err != nil {
		t.Fatal(err)
	}

	second := newRig(t, dir, nil)
	if err := second.service.Load(); err != nil {
		t.Fatal(err)
	}
	vessel, ok := second.vessels.Get("V-1")
	if !ok || vessel.Name != "Tank 1" || vessel.Zone != "north" {
		t.Fatalf("vessel=%+v ok=%v", vessel, ok)
	}
	probeState, ok := second.probes.Get("P-1")
	if !ok || probeState.VesselID != "V-1" || probeState.Kind != model.ProbeLevel {
		t.Fatalf("probe=%+v ok=%v", probeState, ok)
	}
	if _, ok := second.circuits.Get("C-1"); ok {
		t.Fatal("deactivated circuit re-entered the runtime registry")
	}
	view := second.service.View()
	if len(view.Vessels) != 2 || len(view.Probes) != 1 || len(view.Circuits) != 1 {
		t.Fatalf("view=%+v", view)
	}
	if view.Circuits[0].Active {
		t.Fatal("deactivated circuit lost its inactive marker")
	}
}

func TestDuplicateIDRejectedAcrossTypes(t *testing.T) {
	r := newRig(t, t.TempDir(), nil)
	if _, err := r.service.RegisterVessel("X-1", "Tank", "north"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.service.RegisterProbe("X-1", model.ProbeLevel, "X-1"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err=%v", err)
	}
	if _, err := r.service.RegisterCircuit("X-1", "north", "VALVE-1"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err=%v", err)
	}
	if len(r.probes.List()) != 0 || len(r.circuits.List()) != 0 {
		t.Fatal("rejected registrations leaked into runtime registries")
	}
}

func TestInvalidRegistrationRejectedWithoutPollutingState(t *testing.T) {
	dir := t.TempDir()
	first := newRig(t, dir, nil)
	if _, err := first.service.RegisterVessel("V-1", "Tank 1", "north"); err != nil {
		t.Fatal(err)
	}
	// Probe bound to a vessel that does not exist.
	if _, err := first.service.RegisterProbe("P-1", model.ProbeLevel, "V-missing"); err == nil {
		t.Fatal("probe with dangling binding was accepted")
	}
	// Circuit in a zone already served by another active circuit.
	if _, err := first.service.RegisterCircuit("C-1", "north", "VALVE-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.service.RegisterCircuit("C-2", "north", "VALVE-2"); err == nil {
		t.Fatal("second circuit in the same zone was accepted")
	}
	if _, err := first.service.RegisterCircuit("C-2", "south", "VALVE-1"); err == nil {
		t.Fatal("second circuit on the same valve was accepted")
	}
	if _, ok := first.probes.Get("P-1"); ok {
		t.Fatal("rejected probe leaked into the runtime registry")
	}
	if _, ok := first.circuits.Get("C-2"); ok {
		t.Fatal("rejected circuit leaked into the runtime registry")
	}
	// The journal must be clean as well: a restart sees none of it.
	second := newRig(t, dir, nil)
	if err := second.service.Load(); err != nil {
		t.Fatal(err)
	}
	view := second.service.View()
	if len(view.Probes) != 0 || len(view.Circuits) != 1 {
		t.Fatalf("rejected configuration reached the journal: %+v", view)
	}
}

func TestDeactivateVesselWithActiveFillRejected(t *testing.T) {
	r := newRig(t, t.TempDir(), nil)
	r.registerAll(t)
	r.startFill(t, "V-1")
	if _, err := r.service.DeactivateVessel("V-1"); !errors.Is(err, ErrOccupied) {
		t.Fatalf("err=%v", err)
	}
	if _, ok := r.vessels.Get("V-1"); !ok {
		t.Fatal("occupied vessel was removed from the runtime registry")
	}
	if !r.service.View().Vessels[0].Active {
		t.Fatal("occupied vessel lost its active marker")
	}
}

func TestDeactivateVesselWithBoundProbeRejected(t *testing.T) {
	r := newRig(t, t.TempDir(), nil)
	r.registerAll(t)
	if _, err := r.service.DeactivateVessel("V-1"); !errors.Is(err, ErrBound) {
		t.Fatalf("err=%v", err)
	}
	if _, ok := r.vessels.Get("V-1"); !ok {
		t.Fatal("vessel with bound probe was removed")
	}
}

func TestDeactivateProbeWithActiveFillRejected(t *testing.T) {
	r := newRig(t, t.TempDir(), nil)
	r.registerAll(t)
	r.startFill(t, "V-1")
	if _, err := r.service.DeactivateProbe("P-1"); !errors.Is(err, ErrOccupied) {
		t.Fatalf("err=%v", err)
	}
	if _, ok := r.probes.Get("P-1"); !ok {
		t.Fatal("probe of a filling vessel was removed")
	}
}

func TestDeactivateAndModifyCircuitWhileReservedRejected(t *testing.T) {
	r := newRig(t, t.TempDir(), nil)
	r.registerAll(t)
	r.startFill(t, "V-1")
	if _, err := r.service.DeactivateCircuit("C-1"); !errors.Is(err, ErrOccupied) {
		t.Fatalf("deactivate err=%v", err)
	}
	if _, err := r.service.ModifyCircuit("C-1", "east", "VALVE-9"); !errors.Is(err, ErrOccupied) {
		t.Fatalf("modify err=%v", err)
	}
	if _, ok := r.circuits.Get("C-1"); !ok {
		t.Fatal("reserved circuit was removed")
	}
}

func TestDeactivatedDevicesStayOutOfRuntimeAfterReload(t *testing.T) {
	dir := t.TempDir()
	first := newRig(t, dir, nil)
	first.registerAll(t)
	if _, err := first.service.DeactivateProbe("P-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.service.DeactivateCircuit("C-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.service.DeactivateVessel("V-1"); err != nil {
		t.Fatal(err)
	}
	if len(first.vessels.List()) != 1 || len(first.probes.List()) != 0 || len(first.circuits.List()) != 0 {
		t.Fatal("deactivated devices remained in runtime registries")
	}
	// Deactivated devices keep their ids reserved.
	if _, err := first.service.RegisterVessel("V-1", "Duplicate", "east"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err=%v", err)
	}
	if _, err := first.service.DeactivateVessel("V-1"); !errors.Is(err, ErrInactive) {
		t.Fatalf("err=%v", err)
	}

	second := newRig(t, dir, nil)
	if err := second.service.Load(); err != nil {
		t.Fatal(err)
	}
	if len(second.vessels.List()) != 1 || len(second.probes.List()) != 0 || len(second.circuits.List()) != 0 {
		t.Fatal("deactivated devices re-entered runtime registries after reload")
	}
	view := second.service.View()
	if len(view.Vessels) != 2 || len(view.Probes) != 1 || len(view.Circuits) != 1 {
		t.Fatalf("view=%+v", view)
	}
	for _, record := range view.Vessels {
		if record.ID == "V-1" && record.Active {
			t.Fatal("deactivated vessel lost its inactive marker")
		}
	}
}

func TestModifyVesselValidatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	first := newRig(t, dir, nil)
	first.registerAll(t)
	if _, err := first.service.ModifyVessel("V-1", "", "east"); err == nil {
		t.Fatal("empty vessel name was accepted")
	}
	if _, err := first.service.ModifyVessel("V-missing", "Tank", "east"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
	record, err := first.service.ModifyVessel("V-1", "Tank 1 rebuilt", "east")
	if err != nil {
		t.Fatal(err)
	}
	if record.Zone != "east" {
		t.Fatalf("record=%+v", record)
	}
	vesselState, ok := first.vessels.Get("V-1")
	if !ok || vesselState.Name != "Tank 1 rebuilt" || vesselState.Zone != "east" || vesselState.Generation != 2 {
		t.Fatalf("vessel=%+v ok=%v", vesselState, ok)
	}

	second := newRig(t, dir, nil)
	if err := second.service.Load(); err != nil {
		t.Fatal(err)
	}
	reloaded, ok := second.vessels.Get("V-1")
	if !ok || reloaded.Name != "Tank 1 rebuilt" || reloaded.Zone != "east" {
		t.Fatalf("reloaded=%+v ok=%v", reloaded, ok)
	}
}

func TestModifyProbeRebindsAndValidatesTarget(t *testing.T) {
	r := newRig(t, t.TempDir(), nil)
	r.registerAll(t)
	if _, err := r.service.ModifyProbe("P-1", model.ProbeTemperature, "V-missing"); err == nil {
		t.Fatal("rebind to a missing vessel was accepted")
	}
	record, err := r.service.ModifyProbe("P-1", model.ProbeTemperature, "V-2")
	if err != nil {
		t.Fatal(err)
	}
	if record.Kind != model.ProbeTemperature || record.VesselID != "V-2" {
		t.Fatalf("record=%+v", record)
	}
	probeState, ok := r.probes.Get("P-1")
	if !ok || probeState.VesselID != "V-2" || probeState.Kind != model.ProbeTemperature || probeState.BindingGeneration != 2 {
		t.Fatalf("probe=%+v ok=%v", probeState, ok)
	}
}

func TestLoadRejectsInvalidPersistedState(t *testing.T) {
	dir := t.TempDir()
	first := newRig(t, dir, nil)
	first.registerAll(t)
	// Simulate a corrupted journal: an event that violates the binding
	// invariant is appended behind the catalog's back.
	bad := ProbeRecord{ID: "P-9", Kind: model.ProbeLevel, VesselID: "V-missing", Active: true, UpdatedAt: time.Unix(1_700_000_000, 0)}
	event, err := model.NewEvent(Partition, "catalog.probe.registered", bad.ID, bad, time.Unix(1_700_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.store.Append(event); err != nil {
		t.Fatal(err)
	}

	second := newRig(t, dir, nil)
	if err := second.service.Load(); err == nil {
		t.Fatal("invalid persisted catalog was loaded")
	}
	if len(second.vessels.List()) != 0 || len(second.probes.List()) != 0 || len(second.circuits.List()) != 0 {
		t.Fatal("invalid persisted catalog polluted the runtime registries")
	}
}
