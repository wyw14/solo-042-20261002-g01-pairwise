package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/wyw14/cry-98/internal/alarm"
	"github.com/wyw14/cry-98/internal/api"
	"github.com/wyw14/cry-98/internal/catalog"
	"github.com/wyw14/cry-98/internal/circuit"
	"github.com/wyw14/cry-98/internal/fill"
	"github.com/wyw14/cry-98/internal/journal"
	"github.com/wyw14/cry-98/internal/model"
	"github.com/wyw14/cry-98/internal/monitor"
	"github.com/wyw14/cry-98/internal/notify"
	"github.com/wyw14/cry-98/internal/pressure"
	"github.com/wyw14/cry-98/internal/probe"
	"github.com/wyw14/cry-98/internal/telemetry"
	"github.com/wyw14/cry-98/internal/transfer"
	"github.com/wyw14/cry-98/internal/valve"
	"github.com/wyw14/cry-98/internal/vessel"
)

type runtime struct {
	server       *http.Server
	retryQueue   *notify.RetryQueue
	dispatcher   *notify.Dispatcher
	alarmService *alarm.Service
}

func buildRuntime(cfg config, now func() time.Time) (*runtime, error) {
	if now == nil {
		now = time.Now
	}
	store, err := journal.NewStore(filepath.Join(cfg.DataDir, "journal"))
	if err != nil {
		return nil, err
	}
	partitions, err := store.Partitions()
	if err != nil {
		return nil, err
	}
	slog.Info("journal opened", "root", store.Root(), "partitions", len(partitions))

	vesselRegistry := vessel.NewRegistry()
	probeRegistry := probe.NewRegistry(256)
	circuitRegistry := circuit.NewRegistry()

	snapshots := telemetry.NewSnapshotStore()
	vesselState := vessel.NewStateService(vesselRegistry, now)
	ingestor := telemetry.NewIngestor(probeRegistry, vesselState, snapshots)
	subscriptions := telemetry.NewSubscriptionManager(probeRegistry, ingestor)

	pressureMonitor := pressure.NewMonitor(now)
	policy := pressure.DefaultPolicy()
	if err := policy.Validate(); err != nil {
		return nil, err
	}

	circuitManager := circuit.NewManager()
	transport := valve.NewMemoryTransport(now)
	valveController, err := valve.NewController(transport, now)
	if err != nil {
		return nil, err
	}
	commandService := valve.NewCommandService(valveController, now)
	routeService := valve.NewRouteService(commandService)
	fillCoordinator := fill.NewCoordinator(circuitManager, circuitRegistry, routeService, vesselState, now)

	// The device catalog is the single source of truth for vessels,
	// probes and circuits. It is loaded from disk (and seeded on first
	// start), validated as a whole, and reconciled into the registries.
	deviceCatalog := catalog.NewService(
		catalog.NewFileStore(filepath.Join(cfg.DataDir, "devices.json")),
		vesselRegistry,
		probeRegistry,
		circuitRegistry,
		fillOccupancy{coordinator: fillCoordinator},
		catalog.Hooks{
			ProbeAttached: func(item model.Probe) error { return subscriptions.Start(item.ID) },
			ProbeDetaching: func(id string) {
				subscriptions.Stop(id)
			},
			ProbeRebound:  rebindWithSubscriptions(probeRegistry, subscriptions, now),
			VesselTracked: trackVessel(pressureMonitor, snapshots, policy, now),
		},
		now,
	)
	if err := deviceCatalog.Load(); err != nil {
		return nil, err
	}
	slog.Info("device catalog loaded", "devices", len(deviceCatalog.List()))
	snapshots.Replace(vesselRegistry.List(), now())

	receiver := telemetry.NewReceiver(probeRegistry)
	probeLifecycle := probe.NewLifecycle(probeRegistry, now)
	probeMonitor := monitor.NewProbeMonitor(probeRegistry, cfg.ProbeStaleAfter, now)
	if err := ingestor.AddObserver(probeMonitor.Observe); err != nil {
		return nil, err
	}
	rebind := transfer.NewRebindService(probeLifecycle, probeRegistry, subscriptions, now)
	transferService := transfer.NewService(rebind, now)

	calibrations := probe.NewCalibrationService(probeRegistry, now)
	if err := calibrations.Subscribe(func(notice probe.CalibrationNotice) {
		_ = pressureMonitor.Reset(notice.VesselID, notice.NewEpoch)
	}); err != nil {
		return nil, err
	}
	if err := ingestor.AddObserver(func(sample model.Sample) {
		if sample.Kind == model.ProbePressure {
			_ = pressureMonitor.Add(sample)
		}
	}); err != nil {
		return nil, err
	}

	circuitHealth := circuit.NewHealthService(circuitRegistry, circuitManager, now)
	retryService, err := fill.NewRetryService(commandService, fill.RetryPolicy{Attempts: 3, Delay: 100 * time.Millisecond})
	if err != nil {
		return nil, err
	}
	fillCoordinator.SetRetryService(retryService)
	completion := fill.NewCompletionService(fillCoordinator, pressureMonitor, store, now)
	aborts := fill.NewAbortService(fillCoordinator, now)
	fillService, err := fill.NewService(fillCoordinator, completion, aborts, 2.1)
	if err != nil {
		return nil, err
	}
	if err := ingestor.AddObserver(func(sample model.Sample) {
		if sample.Kind == model.ProbeLevel && sample.Value < 20 {
			_, _ = fillService.OnLowLevel(context.Background(), sample.VesselID, sample.ObservedAt)
		}
	}); err != nil {
		return nil, err
	}

	siren, err := notify.NewLocalChannel("siren", true)
	if err != nil {
		return nil, err
	}
	terminal, err := notify.NewLocalChannel("duty-terminal", true)
	if err != nil {
		return nil, err
	}
	dispatcher, err := notify.NewDispatcher(now, siren, terminal)
	if err != nil {
		return nil, err
	}
	retryQueue := notify.NewRetryQueue(cfg.NotificationRetry, now)
	delivery, err := alarm.NewDeliveryService(dispatcher, retryQueue, store, now)
	if err != nil {
		return nil, err
	}
	alarmService := alarm.NewService(delivery, now)
	if err := alarm.NewRecovery(store, alarmService).Restore(); err != nil {
		return nil, err
	}
	suppressions := alarm.NewSuppressionService(store, now)
	healthService := monitor.NewHealthService(ingestor, circuitHealth, probeMonitor, now)

	apiServer := api.NewServer(api.Dependencies{
		Catalog:        deviceCatalog,
		Vessels:        vesselRegistry,
		Fills:          fillService,
		Alarms:         alarmService,
		Probes:         probeRegistry,
		ProbeLifecycle: probeLifecycle,
		Transfers:      transferService,
		Telemetry:      ingestor,
		Receiver:       receiver,
		Health:         healthService,
		ProbeMonitor:   probeMonitor,
		Suppressions:   suppressions,
		Now:            now,
	})
	server := &http.Server{
		Addr:              cfg.Address,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return &runtime{server: server, retryQueue: retryQueue, dispatcher: dispatcher, alarmService: alarmService}, nil
}

// fillOccupancy adapts the fill coordinator to the catalog's occupancy
// guard so devices held by an active fill cannot be deactivated.
type fillOccupancy struct {
	coordinator *fill.Coordinator
}

func (o fillOccupancy) GuardVessel(vesselID string, action func() error) error {
	err := o.coordinator.GuardVessel(vesselID, action)
	if errors.Is(err, fill.ErrDeviceOccupied) {
		return catalog.ErrDeviceOccupied
	}
	return err
}

func (o fillOccupancy) GuardCircuit(circuitID string, action func() error) error {
	err := o.coordinator.GuardCircuit(circuitID, action)
	if errors.Is(err, fill.ErrDeviceOccupied) {
		return catalog.ErrDeviceOccupied
	}
	return err
}

// rebindWithSubscriptions moves a probe to another vessel while keeping
// its telemetry subscription consistent, mirroring transfer.RebindService.
func rebindWithSubscriptions(probeRegistry *probe.Registry, subscriptions *telemetry.SubscriptionManager, now func() time.Time) func(string, string) error {
	lifecycle := probe.NewLifecycle(probeRegistry, now)
	return func(probeID, vesselID string) error {
		current, ok := probeRegistry.Get(probeID)
		if !ok {
			return errors.New("probe not found")
		}
		if current.VesselID == vesselID {
			return nil
		}
		subscriptions.Stop(probeID)
		if _, err := lifecycle.Rebind(probeID, vesselID); err != nil {
			return err
		}
		if err := subscriptions.Start(probeID); err != nil {
			_, _ = lifecycle.Rebind(probeID, current.VesselID)
			_ = subscriptions.Start(probeID)
			return err
		}
		return nil
	}
}

// trackVessel keeps derived per-vessel runtime state (pressure windows and
// telemetry snapshots) in step with catalog membership.
func trackVessel(pressureMonitor *pressure.Monitor, snapshots *telemetry.SnapshotStore, policy pressure.Policy, now func() time.Time) func(model.Vessel, bool) {
	return func(item model.Vessel, added bool) {
		if added {
			if err := pressureMonitor.AddVessel(item.ID, policy.RequiredSamples, policy.MaximumSpread); err != nil {
				slog.Error("pressure window setup failed", "vessel", item.ID, "error", err)
			}
			snapshots.Apply(item)
			return
		}
		snapshots.Remove(item.ID)
	}
}

func (r *runtime) run(ctx context.Context) error {
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- r.server.ListenAndServe()
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return r.server.Shutdown(shutdownCtx)
		case err := <-serveErrors:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
			_ = r.retryQueue.RunDue(ctx, r.dispatcher)
			if pending := len(r.retryQueue.Pending()); pending > 0 {
				slog.Warn("alarm notifications awaiting retry", "count", pending)
			}
		}
	}
}
