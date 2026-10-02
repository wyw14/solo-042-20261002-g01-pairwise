package catalog_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wyw14/cry-98/internal/catalog"
	"github.com/wyw14/cry-98/internal/circuit"
	"github.com/wyw14/cry-98/internal/fill"
	"github.com/wyw14/cry-98/internal/probe"
	"github.com/wyw14/cry-98/internal/valve"
	"github.com/wyw14/cry-98/internal/vessel"
)

// TestDeactivationRacesFillStart hammers vessel deactivation against fill
// startup. With a non-atomic check-then-remove, a fill session could be
// started in the window and then have its vessel ripped away, which makes
// the subsequent abort fail. The occupancy guard must prevent that: every
// session that starts must be abortable, and removal only ever happens
// while no session is active.
func TestDeactivationRacesFillStart(t *testing.T) {
	now := time.Unix(9000, 0)
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

	const iterations = 200
	var wg sync.WaitGroup
	var failuresMu sync.Mutex
	var failures []string

	// Filler: start and immediately abort a session. A vessel ripped out
	// mid-session surfaces as an abort failure.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range iterations {
			session, err := coordinator.StartManual(context.Background(), "V-03", 2.1)
			if err != nil {
				continue
			}
			if _, err := aborts.Abort(context.Background(), session.ID, "race cleanup"); err != nil {
				failuresMu.Lock()
				failures = append(failures, "abort after start failed: "+err.Error())
				failuresMu.Unlock()
			}
		}
	}()

	// Deactivator: keeps trying to retire the vessel; once it wins, the
	// vessel stays deactivated and all later fills must fail cleanly.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range iterations {
			_, _ = service.Deactivate("V-03")
		}
	}()

	wg.Wait()
	for _, message := range failures {
		t.Error(message)
	}

	// Final consistency: no live session and either the vessel is present
	// (deactivation never won) or cleanly removed (it won while idle).
	if coordinator.ActiveVessel("V-03") {
		t.Fatal("fill session remained active after race")
	}
}
