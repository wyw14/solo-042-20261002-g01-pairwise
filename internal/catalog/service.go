package catalog

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/wyw14/cry-98/internal/circuit"
	"github.com/wyw14/cry-98/internal/model"
	"github.com/wyw14/cry-98/internal/probe"
	"github.com/wyw14/cry-98/internal/vessel"
)

// Store persists catalog documents.
type Store interface {
	Save(Config) error
	Load() (Config, bool, error)
}

// Occupancy serializes deactivation against fill startup. The guard runs
// the whole deactivation while holding the fill coordinator's lock, so a
// session can never start between the occupancy check and the registry
// removal.
type Occupancy interface {
	// GuardVessel runs action only when no live fill uses the vessel; a
	// probe deactivation guards the vessel it is bound to.
	GuardVessel(vesselID string, action func() error) error
	// GuardCircuit runs action only when the circuit is neither reserved
	// nor retained for fault recovery.
	GuardCircuit(circuitID string, action func() error) error
}

// Hooks receive notifications when devices enter or leave the running
// registries, so callers can manage subscriptions, pressure windows and
// other derived runtime state in step with the catalog.
type Hooks struct {
	// ProbeAttached starts telemetry consumption for a newly active probe.
	ProbeAttached func(model.Probe) error
	// ProbeDetaching stops telemetry consumption before a probe is removed.
	ProbeDetaching func(string)
	// ProbeRebound moves a live probe to another vessel, restarting its
	// subscription so queued samples cannot cross the binding boundary.
	ProbeRebound func(probeID, vesselID string) error
	// VesselTracked is called when an active vessel is added (added=true)
	// or removed (added=false) from the runtime.
	VesselTracked func(model.Vessel, bool)
}

// Service owns the authoritative device catalog and reconciles the vessel,
// probe and circuit registries with it. Mutations are validated and
// persisted as a unit: an invalid configuration is rejected before any
// registry is touched, and a failed runtime reconciliation is rolled back.
type Service struct {
	mu        sync.Mutex
	config    Config
	store     Store
	vessels   *vessel.Registry
	probes    *probe.Registry
	circuits  *circuit.Registry
	occupancy Occupancy
	hooks     Hooks
	now       func() time.Time
}

func NewService(store Store, vessels *vessel.Registry, probes *probe.Registry,
	circuits *circuit.Registry, occupancy Occupancy, hooks Hooks, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{
		store:     store,
		vessels:   vessels,
		probes:    probes,
		circuits:  circuits,
		occupancy: occupancy,
		hooks:     hooks,
		now:       now,
	}
}

// Load reads the persisted catalog (or seeds the built-in default catalog
// on first start), validates it wholesale, and installs the active devices
// into the running registries. It must be called once before serving.
func (s *Service) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	config, found, err := s.store.Load()
	if err != nil {
		return fmt.Errorf("load device catalog: %w", err)
	}
	if !found {
		config = DefaultConfig(s.now())
		if err := s.store.Save(config); err != nil {
			return fmt.Errorf("seed device catalog: %w", err)
		}
	} else {
		if err := validateConfig(config); err != nil {
			return fmt.Errorf("persisted device catalog is invalid: %w", err)
		}
	}
	if err := s.apply(config); err != nil {
		return fmt.Errorf("install device catalog: %w", err)
	}
	s.config = config
	return nil
}

// List returns all catalog entries sorted by id.
func (s *Service) List() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	devices := make([]Device, len(s.config.Devices))
	copy(devices, s.config.Devices)
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	return devices
}

// Get returns a single catalog entry.
func (s *Service) Get(id string) (Device, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findLocked(trim(id))
}

func (s *Service) findLocked(id string) (Device, bool) {
	for _, device := range s.config.Devices {
		if device.ID == id {
			return device, true
		}
	}
	return Device{}, false
}

// Register adds a new device to the catalog.
func (s *Service) Register(request DeviceRequest) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	request = normalizeRequest(request)
	if request.ID == "" {
		return Device{}, errors.New("device id is required")
	}
	if !request.Kind.valid() {
		return Device{}, errors.New("unknown device type")
	}
	if _, exists := s.findLocked(request.ID); exists {
		return Device{}, fmt.Errorf("device %s is already registered", request.ID)
	}

	candidate := s.cloneConfigLocked()
	device := Device{
		ID:        request.ID,
		Kind:      request.Kind,
		Status:    StatusActive,
		Name:      request.Name,
		Zone:      request.Zone,
		ProbeKind: request.ProbeKind,
		VesselID:  request.VesselID,
		ValveID:   request.ValveID,
		Enabled:   request.Enabled,
		UpdatedAt: s.now().UTC(),
	}
	candidate.Devices = append(candidate.Devices, device)
	if err := s.commitLocked(candidate); err != nil {
		return Device{}, err
	}
	return device, nil
}

// Update modifies an existing device. The device kind and identity are
// immutable; everything else may be amended. Probe rebinds go through the
// same whole-config validation as any other change.
func (s *Service) Update(id string, request DeviceRequest) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id = trim(id)

	current, ok := s.findLocked(id)
	if !ok {
		return Device{}, ErrDeviceNotFound
	}
	request = normalizeRequest(request)
	if request.Kind != "" && request.Kind != current.Kind {
		return Device{}, errors.New("device type is immutable")
	}

	next := current
	if request.Name != "" {
		next.Name = request.Name
	}
	if request.Zone != "" {
		next.Zone = request.Zone
	}
	if request.ProbeKind.Valid() {
		next.ProbeKind = request.ProbeKind
	}
	if request.VesselID != "" {
		next.VesselID = request.VesselID
	}
	if request.ValveID != "" {
		next.ValveID = request.ValveID
	}
	if request.Enabled != nil {
		next.Enabled = request.Enabled
	}
	next.UpdatedAt = s.now().UTC()

	candidate := s.replaceLocked(current, next)
	if err := s.commitLocked(candidate); err != nil {
		return Device{}, err
	}
	return next, nil
}

// Deactivate retires a device without deleting its catalog record. Devices
// held by an active fill cannot be deactivated, and an active probe's
// bound vessel cannot be deactivated while the fill is live either.
func (s *Service) Deactivate(id string) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id = trim(id)

	current, ok := s.findLocked(id)
	if !ok {
		return Device{}, ErrDeviceNotFound
	}
	if !current.active() {
		return current, nil
	}

	next := current
	next.Status = StatusDeactivated
	next.UpdatedAt = s.now().UTC()
	candidate := s.replaceLocked(current, next)

	// Validate, reconcile and persist as one unit inside the fill guard
	// so an active session can never interleave with the removal, and an
	// illegal configuration is rejected without touching the runtime.
	commit := func() error {
		if err := validateConfig(candidate); err != nil {
			return err
		}
		return s.commitValidatedLocked(candidate)
	}
	if s.occupancy != nil {
		switch current.Kind {
		case KindVessel:
			inner := commit
			commit = func() error { return s.occupancy.GuardVessel(id, inner) }
		case KindProbe:
			inner := commit
			commit = func() error { return s.occupancy.GuardVessel(current.VesselID, inner) }
		case KindCircuit:
			inner := commit
			commit = func() error { return s.occupancy.GuardCircuit(id, inner) }
		}
	}
	if err := commit(); err != nil {
		return Device{}, err
	}
	return next, nil
}

func normalizeRequest(request DeviceRequest) DeviceRequest {
	request.ID = trim(request.ID)
	request.Name = trim(request.Name)
	request.Zone = trim(request.Zone)
	request.VesselID = trim(request.VesselID)
	request.ValveID = trim(request.ValveID)
	return request
}

func (s *Service) cloneConfigLocked() Config {
	devices := make([]Device, len(s.config.Devices))
	copy(devices, s.config.Devices)
	return Config{Version: s.config.Version, UpdatedAt: s.config.UpdatedAt, Devices: devices}
}

func (s *Service) replaceLocked(current, next Device) Config {
	candidate := s.cloneConfigLocked()
	for index := range candidate.Devices {
		if candidate.Devices[index].ID == current.ID {
			candidate.Devices[index] = next
			break
		}
	}
	return candidate
}

// commitLocked validates the candidate catalog, reconciles the running
// registries with it, and only then persists. A failure at any stage rolls
// the runtime back to the previous configuration, so an illegal change can
// never leave partial state behind.
func (s *Service) commitLocked(candidate Config) error {
	if err := validateConfig(candidate); err != nil {
		return err
	}
	return s.commitValidatedLocked(candidate)
}

// commitValidatedLocked performs the reconcile-and-persist steps for a
// catalog that has already passed whole-config validation. It is also run
// inside the fill occupancy guard during deactivation.
func (s *Service) commitValidatedLocked(candidate Config) error {
	rollback, err := s.reconcile(s.config, candidate)
	if err != nil {
		rollback()
		return err
	}
	candidate.Version = s.config.Version + 1
	candidate.UpdatedAt = s.now().UTC()
	if err := s.store.Save(candidate); err != nil {
		rollback()
		return fmt.Errorf("persist device catalog: %w", err)
	}
	s.config = candidate
	return nil
}

// apply installs a catalog into empty registries. Used at startup where
// the previous configuration is empty and there is nothing to roll back
// to; any failure is still reported wholesale.
func (s *Service) apply(config Config) error {
	rollback, err := s.reconcile(Config{}, config)
	if err != nil {
		rollback()
		return err
	}
	return nil
}

// operation is one reversible step in a runtime reconciliation.
type operation struct {
	apply    func() error
	rollback func()
}

// reconcile computes the difference between two catalogs and applies the
// needed registry changes, returning a rollback function that reverses
// every successful step.
func (s *Service) reconcile(previous, next Config) (func(), error) {
	previousByID := make(map[string]Device, len(previous.Devices))
	for _, device := range previous.Devices {
		previousByID[device.ID] = device
	}

	var operations []operation
	for _, device := range next.Devices {
		old, existed := previousByID[device.ID]
		switch {
		case !existed && device.active():
			operations = append(operations, s.activateOps(device)...)
		case existed && !old.active() && device.active():
			operations = append(operations, s.activateOps(device)...)
		case existed && old.active() && !device.active():
			operations = append(operations, s.deactivateOps(old)...)
		case existed && old.active() && device.active():
			operations = append(operations, s.updateOps(old, device)...)
		}
	}
	// Devices present before but absent from the next catalog are removed.
	nextByID := make(map[string]Device, len(next.Devices))
	for _, device := range next.Devices {
		nextByID[device.ID] = device
	}
	for _, old := range previous.Devices {
		if _, retained := nextByID[old.ID]; !retained && old.active() {
			operations = append(operations, s.deactivateOps(old)...)
		}
	}

	applied := make([]operation, 0, len(operations))
	for _, op := range operations {
		if err := op.apply(); err != nil {
			for index := len(applied) - 1; index >= 0; index-- {
				applied[index].rollback()
			}
			return func() {}, err
		}
		applied = append(applied, op)
	}
	return func() {
		for index := len(applied) - 1; index >= 0; index-- {
			applied[index].rollback()
		}
	}, nil
}

func (s *Service) activateOps(device Device) []operation {
	switch device.Kind {
	case KindVessel:
		var registered model.Vessel
		return []operation{{
			apply: func() error {
				item, err := model.NewVessel(device.ID, device.Name, device.Zone, s.now())
				if err != nil {
					return err
				}
				if err := s.vessels.Register(item); err != nil {
					return err
				}
				registered = item
				if s.hooks.VesselTracked != nil {
					s.hooks.VesselTracked(item, true)
				}
				return nil
			},
			rollback: func() {
				if s.vessels.Remove(device.ID) && s.hooks.VesselTracked != nil {
					s.hooks.VesselTracked(registered, false)
				}
			},
		}}
	case KindProbe:
		return []operation{{
			apply: func() error {
				item, err := model.NewProbe(device.ID, device.ProbeKind, device.VesselID, s.now())
				if err != nil {
					return err
				}
				if err := s.probes.Register(item); err != nil {
					return err
				}
				if s.hooks.ProbeAttached != nil {
					if err := s.hooks.ProbeAttached(item); err != nil {
						if s.hooks.ProbeDetaching != nil {
							s.hooks.ProbeDetaching(device.ID)
						}
						_ = s.probes.Remove(device.ID)
						return err
					}
				}
				return nil
			},
			rollback: func() {
				if s.hooks.ProbeDetaching != nil {
					s.hooks.ProbeDetaching(device.ID)
				}
				s.probes.Remove(device.ID)
			},
		}}
	case KindCircuit:
		return []operation{{
			apply: func() error {
				enabled := true
				if device.Enabled != nil {
					enabled = *device.Enabled
				}
				return s.circuits.Register(circuit.Circuit{
					ID:      device.ID,
					Zone:    device.Zone,
					ValveID: device.ValveID,
					Enabled: enabled,
				})
			},
			rollback: func() { s.circuits.Remove(device.ID) },
		}}
	}
	return nil
}

func (s *Service) deactivateOps(device Device) []operation {
	switch device.Kind {
	case KindVessel:
		return []operation{{
			apply: func() error {
				item, ok := s.vessels.Get(device.ID)
				if !ok {
					return nil
				}
				if s.vessels.Remove(device.ID) && s.hooks.VesselTracked != nil {
					s.hooks.VesselTracked(item, false)
				}
				return nil
			},
			rollback: func() {
				item, err := model.NewVessel(device.ID, device.Name, device.Zone, s.now())
				if err != nil {
					return
				}
				if err := s.vessels.Register(item); err == nil && s.hooks.VesselTracked != nil {
					s.hooks.VesselTracked(item, true)
				}
			},
		}}
	case KindProbe:
		return []operation{{
			apply: func() error {
				if s.hooks.ProbeDetaching != nil {
					s.hooks.ProbeDetaching(device.ID)
				}
				s.probes.Remove(device.ID)
				return nil
			},
			rollback: func() {
				item, err := model.NewProbe(device.ID, device.ProbeKind, device.VesselID, s.now())
				if err != nil {
					return
				}
				if err := s.probes.Register(item); err != nil {
					return
				}
				if s.hooks.ProbeAttached != nil {
					_ = s.hooks.ProbeAttached(item)
				}
			},
		}}
	case KindCircuit:
		return []operation{{
			apply: func() error {
				s.circuits.Remove(device.ID)
				return nil
			},
			rollback: func() {
				enabled := true
				if device.Enabled != nil {
					enabled = *device.Enabled
				}
				_ = s.circuits.Register(circuit.Circuit{
					ID:      device.ID,
					Zone:    device.Zone,
					ValveID: device.ValveID,
					Enabled: enabled,
				})
			},
		}}
	}
	return nil
}

func (s *Service) updateOps(old, next Device) []operation {
	switch next.Kind {
	case KindVessel:
		changed := old.Name != next.Name || old.Zone != next.Zone
		if !changed {
			return nil
		}
		return []operation{{
			apply: func() error {
				_, err := s.vessels.Update(next.ID, func(current model.Vessel) (model.Vessel, error) {
					current.Name = next.Name
					current.Zone = next.Zone
					current.UpdatedAt = s.now().UTC()
					return current, nil
				})
				return err
			},
			rollback: func() {
				_, _ = s.vessels.Update(next.ID, func(current model.Vessel) (model.Vessel, error) {
					current.Name = old.Name
					current.Zone = old.Zone
					return current, nil
				})
			},
		}}
	case KindProbe:
		var ops []operation
		if old.ProbeKind != next.ProbeKind {
			ops = append(ops, operation{
				apply: func() error {
					_, err := s.probes.Update(next.ID, func(current model.Probe) (model.Probe, error) {
						current.Kind = next.ProbeKind
						current.UpdatedAt = s.now().UTC()
						return current, nil
					})
					return err
				},
				rollback: func() {
					_, _ = s.probes.Update(next.ID, func(current model.Probe) (model.Probe, error) {
						current.Kind = old.ProbeKind
						return current, nil
					})
				},
			})
		}
		if old.VesselID != next.VesselID {
			ops = append(ops, operation{
				apply: func() error {
					if s.hooks.ProbeRebound == nil {
						_, err := s.probes.Update(next.ID, func(current model.Probe) (model.Probe, error) {
							return current.Rebind(next.VesselID, s.now())
						})
						return err
					}
					return s.hooks.ProbeRebound(next.ID, next.VesselID)
				},
				rollback: func() {
					if s.hooks.ProbeRebound != nil {
						_ = s.hooks.ProbeRebound(next.ID, old.VesselID)
					} else {
						_, _ = s.probes.Update(next.ID, func(current model.Probe) (model.Probe, error) {
							return current.Rebind(old.VesselID, s.now())
						})
					}
				},
			})
		}
		return ops
	case KindCircuit:
		changed := old.Zone != next.Zone || old.ValveID != next.ValveID ||
			(next.Enabled != nil && (old.Enabled == nil || *old.Enabled != *next.Enabled))
		if !changed {
			return nil
		}
		return []operation{{
			apply: func() error {
				_, err := s.circuits.Update(next.ID, func(current circuit.Circuit) (circuit.Circuit, error) {
					current.Zone = next.Zone
					current.ValveID = next.ValveID
					if next.Enabled != nil {
						current.Enabled = *next.Enabled
					}
					return current, nil
				})
				return err
			},
			rollback: func() {
				_, _ = s.circuits.Update(next.ID, func(current circuit.Circuit) (circuit.Circuit, error) {
					current.Zone = old.Zone
					current.ValveID = old.ValveID
					if old.Enabled != nil {
						current.Enabled = *old.Enabled
					}
					return current, nil
				})
			},
		}}
	}
	return nil
}

// DefaultConfig returns the devices shipped before any operator-managed
// catalog exists. It is only used to seed a fresh data directory.
func DefaultConfig(now time.Time) Config {
	if now.IsZero() {
		now = time.Now()
	}
	enabled := true
	devices := []Device{
		{ID: "V-03", Kind: KindVessel, Status: StatusActive, Name: "Cryogenic vessel 03", Zone: "north", UpdatedAt: now.UTC()},
		{ID: "V-07", Kind: KindVessel, Status: StatusActive, Name: "Cryogenic vessel 07", Zone: "north", UpdatedAt: now.UTC()},
		{ID: "V-09", Kind: KindVessel, Status: StatusActive, Name: "Cryogenic vessel 09", Zone: "south", UpdatedAt: now.UTC()},
		{ID: "V-12", Kind: KindVessel, Status: StatusActive, Name: "Cryogenic vessel 12", Zone: "south", UpdatedAt: now.UTC()},
		{ID: "V-15", Kind: KindVessel, Status: StatusActive, Name: "Cryogenic vessel 15", Zone: "east", UpdatedAt: now.UTC()},
		{ID: "V-18", Kind: KindVessel, Status: StatusActive, Name: "Cryogenic vessel 18", Zone: "east", UpdatedAt: now.UTC()},
		{ID: "P-L03", Kind: KindProbe, Status: StatusActive, ProbeKind: model.ProbeLevel, VesselID: "V-03", UpdatedAt: now.UTC()},
		{ID: "P-P07", Kind: KindProbe, Status: StatusActive, ProbeKind: model.ProbePressure, VesselID: "V-07", UpdatedAt: now.UTC()},
		{ID: "P-L12", Kind: KindProbe, Status: StatusActive, ProbeKind: model.ProbeLevel, VesselID: "V-12", UpdatedAt: now.UTC()},
		{ID: "P-44", Kind: KindProbe, Status: StatusActive, ProbeKind: model.ProbeTemperature, VesselID: "V-15", UpdatedAt: now.UTC()},
		{ID: "P-P18", Kind: KindProbe, Status: StatusActive, ProbeKind: model.ProbePressure, VesselID: "V-18", UpdatedAt: now.UTC()},
		{ID: "C-A", Kind: KindCircuit, Status: StatusActive, Zone: "north", ValveID: "VALVE-A", Enabled: &enabled, UpdatedAt: now.UTC()},
		{ID: "C-B", Kind: KindCircuit, Status: StatusActive, Zone: "south", ValveID: "VALVE-B", Enabled: &enabled, UpdatedAt: now.UTC()},
	}
	return Config{Version: 1, UpdatedAt: now.UTC(), Devices: devices}
}
