package catalog_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyw14/cry-98/internal/catalog"
	"github.com/wyw14/cry-98/internal/circuit"
	"github.com/wyw14/cry-98/internal/fill"
	"github.com/wyw14/cry-98/internal/probe"
	"github.com/wyw14/cry-98/internal/valve"
	"github.com/wyw14/cry-98/internal/vessel"
)

type fillOccupancy struct{ coordinator *fill.Coordinator }

func (o fillOccupancy) guard(err error) error {
	if errors.Is(err, fill.ErrDeviceOccupied) {
		return catalog.ErrDeviceOccupied
	}
	return err
}

func (o fillOccupancy) GuardVessel(id string, action func() error) error {
	return o.guard(o.coordinator.GuardVessel(id, action))
}

func (o fillOccupancy) GuardCircuit(id string, action func() error) error {
	return o.guard(o.coordinator.GuardCircuit(id, action))
}

// TestActiveFillBlocksDeactivation exercises the rule end to end with the
// real fill coordinator: while a vessel is being filled, the vessel, its
// reserved circuit and its serving probes may not be deactivated.
func TestActiveFillBlocksDeactivation(t *testing.T) {
	now := time.Unix(5000, 0)
	clock := func() time.Time { return now }
	vessels := vessel.NewRegistry()
	probes := probe.NewRegistry(8)
	circuits := circuit.NewRegistry()

	controller, err := valve.NewController(valve.NewMemoryTransport(clock), clock)
	if err != nil {
		t.Fatal(err)
	}
	routes := valve.NewRouteService(valve.NewCommandService(controller, clock))
	coordinator := fill.NewCoordinator(circuit.NewManager(), circuits, routes, vessel.NewStateService(vessels, clock), clock)
	aborts := fill.NewAbortService(coordinator, clock)

	service := catalog.NewService(
		catalog.NewFileStore(filepath.Join(t.TempDir(), "devices.json")),
		vessels, probes, circuits, fillOccupancy{coordinator: coordinator}, catalog.Hooks{}, clock,
	)
	if err := service.Load(); err != nil {
		t.Fatal(err)
	}

	session, err := coordinator.StartManual(context.Background(), "V-03", 2.1)
	if err != nil {
		t.Fatal(err)
	}
	if session.CircuitID != "C-A" {
		t.Fatalf("selected circuit=%s, want C-A", session.CircuitID)
	}
	if !coordinator.ActiveVessel("V-03") || !coordinator.ActiveCircuit("C-A") {
		t.Fatal("fill session did not mark devices occupied")
	}

	if _, err := service.Deactivate("V-03"); err != catalog.ErrDeviceOccupied {
		t.Fatalf("vessel deactivation error=%v", err)
	}
	if _, err := service.Deactivate("C-A"); err != catalog.ErrDeviceOccupied {
		t.Fatalf("circuit deactivation error=%v", err)
	}
	if _, err := service.Deactivate("P-L03"); !errors.Is(err, catalog.ErrDeviceOccupied) {
		t.Fatalf("probe deactivation error=%v", err)
	}

	// Unrelated devices remain manageable while the fill is running. V-09
	// has no bound probe in the default catalog.
	if _, err := service.Deactivate("V-09"); err != nil {
		t.Fatalf("idle vessel could not be deactivated: %v", err)
	}
	if _, ok := vessels.Get("V-09"); ok {
		t.Fatal("idle vessel was not removed after deactivation")
	}
	// And the occupied devices are untouched in the runtime.
	if _, ok := vessels.Get("V-03"); !ok {
		t.Fatal("occupied vessel was removed")
	}
	if _, ok := circuits.Get("C-A"); !ok {
		t.Fatal("occupied circuit was removed")
	}

	// Once the session is aborted and its reservation released, the
	// circuit can be deactivated.
	if _, err := aborts.Abort(context.Background(), session.ID, "test complete"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Deactivate("C-A"); err != nil {
		t.Fatalf("released circuit could not be deactivated: %v", err)
	}
	if _, ok := circuits.Get("C-A"); ok {
		t.Fatal("circuit still present after deactivation")
	}
}
