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
	vesselState := vessel.NewStateService(vesselRegistry, now)

	probeRegistry := probe.NewRegistry(256)
	probeLifecycle := probe.NewLifecycle(probeRegistry, now)
	snapshots := telemetry.NewSnapshotStore()
	ingestor := telemetry.NewIngestor(probeRegistry, vesselState, snapshots)
	subscriptions := telemetry.NewSubscriptionManager(probeRegistry, ingestor)
	receiver := telemetry.NewReceiver(probeRegistry)
	probeMonitor := monitor.NewProbeMonitor(probeRegistry, cfg.ProbeStaleAfter, now)
	if err := ingestor.AddObserver(probeMonitor.Observe); err != nil {
		return nil, err
	}
	rebind := transfer.NewRebindService(probeLifecycle, probeRegistry, subscriptions, now)
	transferService := transfer.NewService(rebind, now)

	pressureMonitor := pressure.NewMonitor(now)
	policy := pressure.DefaultPolicy()
	if err := policy.Validate(); err != nil {
		return nil, err
	}
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

	circuitRegistry := circuit.NewRegistry()
	circuitManager := circuit.NewManager()
	circuitHealth := circuit.NewHealthService(circuitRegistry, circuitManager, now)
	transport := valve.NewMemoryTransport(now)
	valveController, err := valve.NewController(transport, now)
	if err != nil {
		return nil, err
	}
	commandService := valve.NewCommandService(valveController, now)
	routeService := valve.NewRouteService(commandService)
	fillCoordinator := fill.NewCoordinator(circuitManager, circuitRegistry, routeService, vesselState, now)
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

	catalogService, err := catalog.NewService(catalog.Dependencies{
		Vessels:  vesselRegistry,
		Probes:   probeRegistry,
		Circuits: circuitRegistry,
		Manager:  circuitManager,
		Fills:    fillCoordinator,
		Journal:  store,
		Effects: catalog.Effects{
			VesselApplied: func(vessel model.Vessel) {
				if err := pressureMonitor.AddVessel(vessel.ID, policy.RequiredSamples, policy.MaximumSpread); err != nil {
					slog.Warn("pressure window not installed", "vessel", vessel.ID, "error", err)
				}
				snapshots.Apply(vessel)
			},
			VesselRemoved: func(vesselID string) {
				snapshots.Remove(vesselID)
			},
			ProbeApplied: func(probe model.Probe) {
				if err := subscriptions.Start(probe.ID); err != nil {
					slog.Warn("probe subscription not started", "probe", probe.ID, "error", err)
				}
			},
			ProbeRemoved: func(probeID string) {
				subscriptions.Stop(probeID)
			},
		},
		Seed: seedCatalog,
		Now:  now,
	})
	if err != nil {
		return nil, err
	}
	if err := catalogService.Load(); err != nil {
		return nil, err
	}
	slog.Info("device catalog loaded", "view", catalogService.View())

	apiServer := api.NewServer(api.Dependencies{
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
		Catalog:        catalogService,
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

// seedCatalog registers the built-in device set through the catalog's
// normal validated and persisted path. It only runs when the persisted
// catalog is empty (first boot).
func seedCatalog(service *catalog.Service) error {
	for _, seed := range []struct{ id, name, zone string }{
		{"V-03", "Cryogenic vessel 03", "north"},
		{"V-07", "Cryogenic vessel 07", "north"},
		{"V-09", "Cryogenic vessel 09", "south"},
		{"V-12", "Cryogenic vessel 12", "south"},
		{"V-15", "Cryogenic vessel 15", "east"},
		{"V-18", "Cryogenic vessel 18", "east"},
	} {
		if _, err := service.RegisterVessel(seed.id, seed.name, seed.zone); err != nil {
			return err
		}
	}
	for _, seed := range []struct {
		id, vesselID string
		kind         model.ProbeKind
	}{
		{"P-L03", "V-03", model.ProbeLevel},
		{"P-P07", "V-07", model.ProbePressure},
		{"P-L12", "V-12", model.ProbeLevel},
		{"P-44", "V-15", model.ProbeTemperature},
		{"P-P18", "V-18", model.ProbePressure},
	} {
		if _, err := service.RegisterProbe(seed.id, seed.kind, seed.vesselID); err != nil {
			return err
		}
	}
	for _, seed := range []struct{ id, zone, valveID string }{
		{"C-A", "north", "VALVE-A"},
		{"C-B", "south", "VALVE-B"},
	} {
		if _, err := service.RegisterCircuit(seed.id, seed.zone, seed.valveID); err != nil {
			return err
		}
	}
	return nil
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
