package catalog

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyw14/cry-98/internal/circuit"
	"github.com/wyw14/cry-98/internal/model"
	"github.com/wyw14/cry-98/internal/probe"
	"github.com/wyw14/cry-98/internal/vessel"
)

type stubOccupancy struct {
	vessels  map[string]bool
	circuits map[string]bool
}

func (s stubOccupancy) GuardVessel(id string, action func() error) error {
	if s.vessels[id] {
		return ErrDeviceOccupied
	}
	return action()
}

func (s stubOccupancy) GuardCircuit(id string, action func() error) error {
	if s.circuits[id] {
		return ErrDeviceOccupied
	}
	return action()
}

type harness struct {
	service  *Service
	vessels  *vessel.Registry
	probes   *probe.Registry
	circuits *circuit.Registry
	hooks    Hooks
	attached []string
	detached []string
}

func newHarness(t *testing.T, store Store, occupancy Occupancy) *harness {
	t.Helper()
	h := &harness{
		vessels:  vessel.NewRegistry(),
		probes:   probe.NewRegistry(8),
		circuits: circuit.NewRegistry(),
	}
	h.hooks = Hooks{
		ProbeAttached: func(item model.Probe) error {
			h.attached = append(h.attached, item.ID)
			return nil
		},
		ProbeDetaching: func(id string) { h.detached = append(h.detached, id) },
	}
	h.service = NewService(store, h.vessels, h.probes, h.circuits, occupancy, h.hooks, func() time.Time {
		return time.Unix(1000, 0)
	})
	return h
}

func boolPtr(value bool) *bool { return &value }

func TestLoadSeedsDefaultCatalogOnFirstStart(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "devices.json"))
	h := newHarness(t, store, nil)
	if err := h.service.Load(); err != nil {
		t.Fatal(err)
	}
	defaults := DefaultConfig(time.Unix(1, 0))
	if len(h.service.List()) != len(defaults.Devices) {
		t.Fatalf("devices=%d want %d", len(h.service.List()), len(defaults.Devices))
	}
	if len(h.vessels.List()) != 6 {
		t.Fatalf("vessels=%d", len(h.vessels.List()))
	}
	if len(h.probes.List()) != 5 {
		t.Fatalf("probes=%d", len(h.probes.List()))
	}
	if len(h.circuits.List()) != 2 {
		t.Fatalf("circuits=%d", len(h.circuits.List()))
	}
}

func TestReloadLoadsPersistedCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	store := NewFileStore(path)
	first := newHarness(t, store, nil)
	if err := first.service.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.service.Deactivate("V-09"); err != nil {
		t.Fatal(err)
	}
	if _, err := first.service.Register(DeviceRequest{
		ID: "V-21", Kind: KindVessel, Name: "New vessel", Zone: "west",
	}); err != nil {
		t.Fatal(err)
	}

	second := newHarness(t, NewFileStore(path), nil)
	if err := second.service.Load(); err != nil {
		t.Fatal(err)
	}
	if _, ok := second.vessels.Get("V-09"); ok {
		t.Fatal("deactivated vessel was restored into the runtime")
	}
	if _, ok := second.vessels.Get("V-21"); !ok {
		t.Fatal("registered vessel missing after restart")
	}
	if _, ok := second.service.Get("V-09"); !ok {
		t.Fatal("deactivated device record disappeared from catalog")
	}
}

func TestRegisterRejectsInvalidBindingWithoutPollutingRuntime(t *testing.T) {
	h := newHarness(t, NewFileStore(filepath.Join(t.TempDir(), "devices.json")), nil)
	if err := h.service.Load(); err != nil {
		t.Fatal(err)
	}
	before := len(h.probes.List())
	_, err := h.service.Register(DeviceRequest{
		ID: "P-BAD", Kind: KindProbe, ProbeKind: model.ProbeLevel, VesselID: "V-404",
	})
	if err == nil {
		t.Fatal("probe bound to unknown vessel was registered")
	}
	if len(h.probes.List()) != before {
		t.Fatal("failed registration changed probe registry")
	}
	if _, ok := h.service.Get("P-BAD"); ok {
		t.Fatal("rejected device entered catalog state")
	}
}

func TestRegisterRejectsDuplicateIDAcrossKinds(t *testing.T) {
	h := newHarness(t, NewFileStore(filepath.Join(t.TempDir(), "devices.json")), nil)
	if err := h.service.Load(); err != nil {
		t.Fatal(err)
	}
	_, err := h.service.Register(DeviceRequest{
		ID: "C-A", Kind: KindVessel, Name: "Sneaky", Zone: "north",
	})
	if err == nil {
		t.Fatal("reusing a circuit id for a vessel was accepted")
	}
}

func TestUpdateRebindsProbeAndRollsBackOnInvalidTarget(t *testing.T) {
	h := newHarness(t, NewFileStore(filepath.Join(t.TempDir(), "devices.json")), nil)
	if err := h.service.Load(); err != nil {
		t.Fatal(err)
	}
	rebound := func(probeID, vesselID string) error {
		lifecycle := probe.NewLifecycle(h.probes, func() time.Time { return time.Unix(1000, 0) })
		_, err := lifecycle.Rebind(probeID, vesselID)
		return err
	}
	h.service.hooks.ProbeRebound = rebound

	probeState, _ := h.probes.Get("P-L03")
	original := probeState.VesselID
	if _, err := h.service.Update("P-L03", DeviceRequest{VesselID: "V-12"}); err != nil {
		t.Fatal(err)
	}
	probeState, _ = h.probes.Get("P-L03")
	if probeState.VesselID != "V-12" {
		t.Fatalf("binding=%s", probeState.VesselID)
	}
	if probeState.BindingGeneration != 2 {
		t.Fatalf("generation=%d", probeState.BindingGeneration)
	}

	if _, err := h.service.Update("P-L03", DeviceRequest{VesselID: "V-404"}); err == nil {
		t.Fatal("rebind to unknown vessel accepted")
	}
	probeState, _ = h.probes.Get("P-L03")
	if probeState.VesselID != "V-12" {
		t.Fatalf("failed rebind polluted binding: %s (was %s)", probeState.VesselID, original)
	}
}

func TestDeactivateRemovesDeviceFromRuntime(t *testing.T) {
	h := newHarness(t, NewFileStore(filepath.Join(t.TempDir(), "devices.json")), nil)
	if err := h.service.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Deactivate("C-B"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.circuits.Get("C-B"); ok {
		t.Fatal("deactivated circuit still in registry")
	}

	if _, err := h.service.Deactivate("P-L12"); err != nil {
		t.Fatal(err)
	}
	if len(h.detached) != 1 || h.detached[0] != "P-L12" {
		t.Fatalf("detached=%v", h.detached)
	}
	if _, ok := h.probes.Get("P-L12"); ok {
		t.Fatal("deactivated probe still in registry")
	}
}

func TestDeactivateRejectsOccupiedVesselAndCircuit(t *testing.T) {
	occupied := stubOccupancy{
		vessels:  map[string]bool{"V-03": true},
		circuits: map[string]bool{"C-A": true},
	}
	h := newHarness(t, NewFileStore(filepath.Join(t.TempDir(), "devices.json")), occupied)
	if err := h.service.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Deactivate("V-03"); !errors.Is(err, ErrDeviceOccupied) {
		t.Fatalf("vessel error=%v", err)
	}
	if _, err := h.service.Deactivate("C-A"); !errors.Is(err, ErrDeviceOccupied) {
		t.Fatalf("circuit error=%v", err)
	}
	if _, ok := h.vessels.Get("V-03"); !ok {
		t.Fatal("occupied vessel was removed despite rejection")
	}
}

func TestDeactivateVesselWithActiveProbeIsRejectedWholesale(t *testing.T) {
	h := newHarness(t, NewFileStore(filepath.Join(t.TempDir(), "devices.json")), nil)
	if err := h.service.Load(); err != nil {
		t.Fatal(err)
	}
	// P-L03 is bound to V-03; deactivating the vessel must fail because
	// the probe would dangle.
	if _, err := h.service.Deactivate("V-03"); err == nil {
		t.Fatal("vessel with a bound active probe was deactivated")
	}
	if _, ok := h.vessels.Get("V-03"); !ok {
		t.Fatal("rejected deactivation removed the vessel anyway")
	}
	if len(h.detached) != 0 {
		t.Fatal("rejected deactivation detached probes")
	}
}

func TestHookFailureRollsBackActivation(t *testing.T) {
	h := newHarness(t, NewFileStore(filepath.Join(t.TempDir(), "devices.json")), nil)
	if err := h.service.Load(); err != nil {
		t.Fatal(err)
	}
	h.service.hooks.ProbeAttached = func(model.Probe) error {
		return errors.New("subscription bus unavailable")
	}
	_, err := h.service.Register(DeviceRequest{
		ID: "P-X1", Kind: KindProbe, ProbeKind: model.ProbeLevel, VesselID: "V-03",
	})
	if err == nil {
		t.Fatal("registration succeeded despite hook failure")
	}
	if _, ok := h.probes.Get("P-X1"); ok {
		t.Fatal("probe remained after hook failure")
	}
	if _, ok := h.service.Get("P-X1"); ok {
		t.Fatal("failed registration entered catalog state")
	}
}

func TestUpdateCircuitKeepsValveUnique(t *testing.T) {
	h := newHarness(t, NewFileStore(filepath.Join(t.TempDir(), "devices.json")), nil)
	if err := h.service.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Update("C-B", DeviceRequest{ValveID: "VALVE-A"}); err == nil {
		t.Fatal("two circuits were allowed to share a valve")
	}
	c, _ := h.circuits.Get("C-B")
	if c.ValveID != "VALVE-B" {
		t.Fatalf("valve=%s after rejected update", c.ValveID)
	}
	if _, err := h.service.Update("C-B", DeviceRequest{ValveID: "VALVE-C", Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	c, _ = h.circuits.Get("C-B")
	if c.ValveID != "VALVE-C" || c.Enabled {
		t.Fatalf("circuit not updated: %+v", c)
	}
}

func TestUpdateRejectsKindChange(t *testing.T) {
	h := newHarness(t, NewFileStore(filepath.Join(t.TempDir(), "devices.json")), nil)
	if err := h.service.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.Update("V-03", DeviceRequest{Kind: KindCircuit, Zone: "north"}); err == nil {
		t.Fatal("device kind change accepted")
	}
}
