package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wyw14/cry-98/internal/circuit"
	"github.com/wyw14/cry-98/internal/fill"
	"github.com/wyw14/cry-98/internal/journal"
	"github.com/wyw14/cry-98/internal/model"
	"github.com/wyw14/cry-98/internal/probe"
	"github.com/wyw14/cry-98/internal/vessel"
)

// Effects are best-effort runtime side effects fired after a catalog
// change has been committed. They wire devices into subsystems the
// catalog does not own (telemetry subscriptions, pressure windows,
// vessel snapshots).
type Effects struct {
	VesselApplied func(model.Vessel)
	VesselRemoved func(vesselID string)
	ProbeApplied  func(model.Probe)
	ProbeRemoved  func(probeID string)
}

type Dependencies struct {
	Vessels  *vessel.Registry
	Probes   *probe.Registry
	Circuits *circuit.Registry
	Manager  *circuit.Manager
	Fills    *fill.Coordinator
	Journal  *journal.Store
	Effects  Effects
	// Seed is invoked once when the persisted catalog is empty, so the
	// deployment can register its initial devices through the normal
	// validated and persisted path.
	Seed func(*Service) error
	Now  func() time.Time
}

// Service owns the device catalog. All mutations take the service lock,
// are validated against a cloned prospective state, persisted to the
// journal, and only then applied to the runtime registries — a rejected
// change therefore leaves both the journal and the runtime untouched.
type Service struct {
	mu       sync.Mutex
	vessels  *vessel.Registry
	probes   *probe.Registry
	circuits *circuit.Registry
	manager  *circuit.Manager
	fills    *fill.Coordinator
	journal  *journal.Store
	effects  Effects
	seed     func(*Service) error
	now      func() time.Time
	state    State
}

func NewService(deps Dependencies) (*Service, error) {
	if deps.Vessels == nil || deps.Probes == nil || deps.Circuits == nil || deps.Manager == nil || deps.Fills == nil || deps.Journal == nil {
		return nil, errors.New("catalog dependencies are incomplete")
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Service{
		vessels:  deps.Vessels,
		probes:   deps.Probes,
		circuits: deps.Circuits,
		manager:  deps.Manager,
		fills:    deps.Fills,
		journal:  deps.Journal,
		effects:  deps.Effects,
		seed:     deps.Seed,
		now:      deps.Now,
		state:    emptyState(),
	}, nil
}

// Load restores the catalog from the journal snapshot plus any events
// appended after it, validates the restored state as a whole, and
// registers the active devices into the runtime registries. When the
// persisted catalog is empty the seed callback registers the initial
// devices through the normal persisted path.
func (s *Service) Load() error {
	s.mu.Lock()
	state := emptyState()
	sequence, err := journal.NewReplay(s.journal).Snapshot(Partition, &state)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	state = state.normalize()
	events, err := s.journal.Events(Partition, sequence)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	for _, event := range events {
		if err := fold(&state, event); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	if err := state.validate(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("persisted catalog is invalid: %w", err)
	}
	s.state = state
	applyErr := s.applyLocked()
	empty := state.empty()
	s.mu.Unlock()
	if applyErr != nil {
		return applyErr
	}
	if empty && s.seed != nil {
		if err := s.seed(s); err != nil {
			return fmt.Errorf("catalog seeding failed: %w", err)
		}
	}
	return nil
}

// View returns the full catalog, including deactivated devices.
func (s *Service) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.view()
}

func (s *Service) RegisterVessel(id, name, zone string) (VesselRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := VesselRecord{
		ID:        strings.TrimSpace(id),
		Name:      strings.TrimSpace(name),
		Zone:      strings.TrimSpace(zone),
		Active:    true,
		UpdatedAt: s.now().UTC(),
	}
	if err := record.validate(); err != nil {
		return VesselRecord{}, err
	}
	if s.state.idTaken(record.ID) {
		return VesselRecord{}, ErrDuplicate
	}
	next := s.state.clone()
	next.Vessels[record.ID] = record
	if err := next.validate(); err != nil {
		return VesselRecord{}, err
	}
	vessel, err := model.NewVessel(record.ID, record.Name, record.Zone, s.now())
	if err != nil {
		return VesselRecord{}, err
	}
	if err := s.persistLocked("catalog.vessel.registered", record.ID, record, next); err != nil {
		return VesselRecord{}, err
	}
	if err := s.vessels.Register(vessel); err != nil {
		return VesselRecord{}, err
	}
	s.vesselApplied(vessel)
	return record, nil
}

func (s *Service) ModifyVessel(id, name, zone string) (VesselRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.state.Vessels[strings.TrimSpace(id)]
	if !ok {
		return VesselRecord{}, ErrNotFound
	}
	if !record.Active {
		return VesselRecord{}, ErrInactive
	}
	record.Name = strings.TrimSpace(name)
	record.Zone = strings.TrimSpace(zone)
	record.UpdatedAt = s.now().UTC()
	if err := record.validate(); err != nil {
		return VesselRecord{}, err
	}
	next := s.state.clone()
	next.Vessels[record.ID] = record
	if err := next.validate(); err != nil {
		return VesselRecord{}, err
	}
	if err := s.persistLocked("catalog.vessel.updated", record.ID, record, next); err != nil {
		return VesselRecord{}, err
	}
	updated, err := s.vessels.Update(record.ID, func(current model.Vessel) (model.Vessel, error) {
		current.Name = record.Name
		current.Zone = record.Zone
		current.Generation++
		current.UpdatedAt = s.now().UTC()
		return current, nil
	})
	if err != nil {
		return VesselRecord{}, err
	}
	s.vesselApplied(updated)
	return record, nil
}

func (s *Service) DeactivateVessel(id string) (VesselRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.state.Vessels[strings.TrimSpace(id)]
	if !ok {
		return VesselRecord{}, ErrNotFound
	}
	if !record.Active {
		return VesselRecord{}, ErrInactive
	}
	if s.fills.HasActive(record.ID) {
		return VesselRecord{}, fmt.Errorf("%w: vessel %q", ErrOccupied, record.ID)
	}
	for _, probeRecord := range s.state.Probes {
		if probeRecord.Active && probeRecord.VesselID == record.ID {
			return VesselRecord{}, fmt.Errorf("%w: probe %q", ErrBound, probeRecord.ID)
		}
	}
	record.Active = false
	record.UpdatedAt = s.now().UTC()
	next := s.state.clone()
	next.Vessels[record.ID] = record
	if err := next.validate(); err != nil {
		return VesselRecord{}, err
	}
	if err := s.persistLocked("catalog.vessel.deactivated", record.ID, record, next); err != nil {
		return VesselRecord{}, err
	}
	if err := s.vessels.Remove(record.ID); err != nil {
		return VesselRecord{}, err
	}
	s.vesselRemoved(record.ID)
	return record, nil
}

func (s *Service) RegisterProbe(id string, kind model.ProbeKind, vesselID string) (ProbeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := ProbeRecord{
		ID:        strings.TrimSpace(id),
		Kind:      kind,
		VesselID:  strings.TrimSpace(vesselID),
		Active:    true,
		UpdatedAt: s.now().UTC(),
	}
	if err := record.validate(); err != nil {
		return ProbeRecord{}, err
	}
	if s.state.idTaken(record.ID) {
		return ProbeRecord{}, ErrDuplicate
	}
	next := s.state.clone()
	next.Probes[record.ID] = record
	if err := next.validate(); err != nil {
		return ProbeRecord{}, err
	}
	probeState, err := model.NewProbe(record.ID, record.Kind, record.VesselID, s.now())
	if err != nil {
		return ProbeRecord{}, err
	}
	if err := s.persistLocked("catalog.probe.registered", record.ID, record, next); err != nil {
		return ProbeRecord{}, err
	}
	if err := s.probes.Register(probeState); err != nil {
		return ProbeRecord{}, err
	}
	s.probeApplied(probeState)
	return record, nil
}

func (s *Service) ModifyProbe(id string, kind model.ProbeKind, vesselID string) (ProbeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.state.Probes[strings.TrimSpace(id)]
	if !ok {
		return ProbeRecord{}, ErrNotFound
	}
	if !record.Active {
		return ProbeRecord{}, ErrInactive
	}
	record.Kind = kind
	record.VesselID = strings.TrimSpace(vesselID)
	record.UpdatedAt = s.now().UTC()
	if err := record.validate(); err != nil {
		return ProbeRecord{}, err
	}
	next := s.state.clone()
	next.Probes[record.ID] = record
	if err := next.validate(); err != nil {
		return ProbeRecord{}, err
	}
	if err := s.persistLocked("catalog.probe.updated", record.ID, record, next); err != nil {
		return ProbeRecord{}, err
	}
	updated, err := s.probes.Update(record.ID, func(current model.Probe) (model.Probe, error) {
		next := current
		if next.VesselID != record.VesselID {
			rebound, err := next.Rebind(record.VesselID, s.now())
			if err != nil {
				return model.Probe{}, err
			}
			next = rebound
		}
		next.Kind = record.Kind
		next.UpdatedAt = s.now().UTC()
		return next, nil
	})
	if err != nil {
		return ProbeRecord{}, err
	}
	s.probeApplied(updated)
	return record, nil
}

func (s *Service) DeactivateProbe(id string) (ProbeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.state.Probes[strings.TrimSpace(id)]
	if !ok {
		return ProbeRecord{}, ErrNotFound
	}
	if !record.Active {
		return ProbeRecord{}, ErrInactive
	}
	if s.fills.HasActive(record.VesselID) {
		return ProbeRecord{}, fmt.Errorf("%w: probe %q is bound to vessel %q", ErrOccupied, record.ID, record.VesselID)
	}
	record.Active = false
	record.UpdatedAt = s.now().UTC()
	next := s.state.clone()
	next.Probes[record.ID] = record
	if err := next.validate(); err != nil {
		return ProbeRecord{}, err
	}
	if err := s.persistLocked("catalog.probe.deactivated", record.ID, record, next); err != nil {
		return ProbeRecord{}, err
	}
	if err := s.probes.Remove(record.ID); err != nil {
		return ProbeRecord{}, err
	}
	s.probeRemoved(record.ID)
	return record, nil
}

func (s *Service) RegisterCircuit(id, zone, valveID string) (CircuitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record := CircuitRecord{
		ID:        strings.TrimSpace(id),
		Zone:      strings.TrimSpace(zone),
		ValveID:   strings.TrimSpace(valveID),
		Active:    true,
		UpdatedAt: s.now().UTC(),
	}
	if err := record.validate(); err != nil {
		return CircuitRecord{}, err
	}
	if s.state.idTaken(record.ID) {
		return CircuitRecord{}, ErrDuplicate
	}
	next := s.state.clone()
	next.Circuits[record.ID] = record
	if err := next.validate(); err != nil {
		return CircuitRecord{}, err
	}
	if err := s.persistLocked("catalog.circuit.registered", record.ID, record, next); err != nil {
		return CircuitRecord{}, err
	}
	if err := s.circuits.Register(circuit.Circuit{ID: record.ID, Zone: record.Zone, ValveID: record.ValveID, Enabled: true}); err != nil {
		return CircuitRecord{}, err
	}
	return record, nil
}

func (s *Service) ModifyCircuit(id, zone, valveID string) (CircuitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.state.Circuits[strings.TrimSpace(id)]
	if !ok {
		return CircuitRecord{}, ErrNotFound
	}
	if !record.Active {
		return CircuitRecord{}, ErrInactive
	}
	if _, reserved := s.manager.Get(record.ID); reserved {
		return CircuitRecord{}, fmt.Errorf("%w: circuit %q is reserved", ErrOccupied, record.ID)
	}
	record.Zone = strings.TrimSpace(zone)
	record.ValveID = strings.TrimSpace(valveID)
	record.UpdatedAt = s.now().UTC()
	if err := record.validate(); err != nil {
		return CircuitRecord{}, err
	}
	next := s.state.clone()
	next.Circuits[record.ID] = record
	if err := next.validate(); err != nil {
		return CircuitRecord{}, err
	}
	if err := s.persistLocked("catalog.circuit.updated", record.ID, record, next); err != nil {
		return CircuitRecord{}, err
	}
	if err := s.circuits.UpdateConfig(record.ID, record.Zone, record.ValveID); err != nil {
		return CircuitRecord{}, err
	}
	return record, nil
}

func (s *Service) DeactivateCircuit(id string) (CircuitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.state.Circuits[strings.TrimSpace(id)]
	if !ok {
		return CircuitRecord{}, ErrNotFound
	}
	if !record.Active {
		return CircuitRecord{}, ErrInactive
	}
	if _, reserved := s.manager.Get(record.ID); reserved {
		return CircuitRecord{}, fmt.Errorf("%w: circuit %q is reserved", ErrOccupied, record.ID)
	}
	record.Active = false
	record.UpdatedAt = s.now().UTC()
	next := s.state.clone()
	next.Circuits[record.ID] = record
	if err := next.validate(); err != nil {
		return CircuitRecord{}, err
	}
	if err := s.persistLocked("catalog.circuit.deactivated", record.ID, record, next); err != nil {
		return CircuitRecord{}, err
	}
	if err := s.circuits.Remove(record.ID); err != nil {
		return CircuitRecord{}, err
	}
	return record, nil
}

// persistLocked appends the change event and refreshes the snapshot
// before the in-memory state is swapped, so a persistence failure leaves
// the running catalog untouched.
func (s *Service) persistLocked(kind, aggregateID string, record any, next State) error {
	event, err := model.NewEvent(Partition, kind, aggregateID, record, s.now())
	if err != nil {
		return err
	}
	if _, err := s.journal.Append(event); err != nil {
		return err
	}
	if _, err := s.journal.Snapshot(Partition, next, s.now()); err != nil {
		return err
	}
	s.state = next
	return nil
}

// applyLocked registers every active device into the runtime registries
// in a deterministic order: vessels before the probes bound to them,
// then circuits. Callers must hold s.mu.
func (s *Service) applyLocked() error {
	for _, id := range sortedKeys(s.state.Vessels) {
		record := s.state.Vessels[id]
		if !record.Active {
			continue
		}
		vessel, err := model.NewVessel(record.ID, record.Name, record.Zone, s.now())
		if err != nil {
			return err
		}
		if err := s.vessels.Register(vessel); err != nil {
			return err
		}
		s.vesselApplied(vessel)
	}
	for _, id := range sortedKeys(s.state.Probes) {
		record := s.state.Probes[id]
		if !record.Active {
			continue
		}
		probeState, err := model.NewProbe(record.ID, record.Kind, record.VesselID, s.now())
		if err != nil {
			return err
		}
		if err := s.probes.Register(probeState); err != nil {
			return err
		}
		s.probeApplied(probeState)
	}
	for _, id := range sortedKeys(s.state.Circuits) {
		record := s.state.Circuits[id]
		if !record.Active {
			continue
		}
		if err := s.circuits.Register(circuit.Circuit{ID: record.ID, Zone: record.Zone, ValveID: record.ValveID, Enabled: true}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) vesselApplied(vessel model.Vessel) {
	if s.effects.VesselApplied != nil {
		s.effects.VesselApplied(vessel)
	}
}

func (s *Service) vesselRemoved(vesselID string) {
	if s.effects.VesselRemoved != nil {
		s.effects.VesselRemoved(vesselID)
	}
}

func (s *Service) probeApplied(probe model.Probe) {
	if s.effects.ProbeApplied != nil {
		s.effects.ProbeApplied(probe)
	}
}

func (s *Service) probeRemoved(probeID string) {
	if s.effects.ProbeRemoved != nil {
		s.effects.ProbeRemoved(probeID)
	}
}

// fold applies one persisted catalog event to the state. Every event
// carries the full resulting record, so folding is a plain upsert.
func fold(state *State, event model.Event) error {
	switch {
	case strings.HasPrefix(event.Kind, "catalog.vessel."):
		var record VesselRecord
		if err := json.Unmarshal(event.Payload, &record); err != nil {
			return err
		}
		state.Vessels[record.ID] = record
	case strings.HasPrefix(event.Kind, "catalog.probe."):
		var record ProbeRecord
		if err := json.Unmarshal(event.Payload, &record); err != nil {
			return err
		}
		state.Probes[record.ID] = record
	case strings.HasPrefix(event.Kind, "catalog.circuit."):
		var record CircuitRecord
		if err := json.Unmarshal(event.Payload, &record); err != nil {
			return err
		}
		state.Circuits[record.ID] = record
	}
	return nil
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
