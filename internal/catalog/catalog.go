// Package catalog maintains the administrative device directory of
// CryoSafe: which vessels, probes, and circuits exist, how probes are
// bound to vessels, and how circuits are assigned to zones. The catalog
// is persisted through the journal and is the single source of truth
// that the runtime registries are populated from at boot. Every change
// is validated as a whole before it is persisted or applied, so an
// illegal configuration is rejected in full and never reaches the
// runtime state.
package catalog

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wyw14/cry-98/internal/model"
)

// Partition is the journal partition that stores catalog events and the
// catalog snapshot.
const Partition = "catalog"

var (
	ErrNotFound  = errors.New("device is not registered")
	ErrDuplicate = errors.New("device id is already registered")
	ErrInactive  = errors.New("device is deactivated")
	ErrOccupied  = errors.New("device is occupied by an active fill session")
	ErrBound     = errors.New("vessel still has active probes bound to it")
)

type VesselRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Zone      string    `json:"zone"`
	Active    bool      `json:"active"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ProbeRecord struct {
	ID        string          `json:"id"`
	Kind      model.ProbeKind `json:"kind"`
	VesselID  string          `json:"vessel_id"`
	Active    bool            `json:"active"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type CircuitRecord struct {
	ID        string    `json:"id"`
	Zone      string    `json:"zone"`
	ValveID   string    `json:"valve_id"`
	Active    bool      `json:"active"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (r VesselRecord) validate() error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Name) == "" || strings.TrimSpace(r.Zone) == "" {
		return errors.New("vessel id, name, and zone are required")
	}
	return nil
}

func (r ProbeRecord) validate() error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.VesselID) == "" {
		return errors.New("probe id and vessel id are required")
	}
	if !r.Kind.Valid() {
		return errors.New("unknown probe kind")
	}
	return nil
}

func (r CircuitRecord) validate() error {
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Zone) == "" || strings.TrimSpace(r.ValveID) == "" {
		return errors.New("circuit id, zone, and valve id are required")
	}
	return nil
}

// State is the full device directory, including deactivated devices.
// Device ids are never reused: a deactivated device keeps its id
// reserved so historical records stay unambiguous.
type State struct {
	Vessels  map[string]VesselRecord  `json:"vessels"`
	Probes   map[string]ProbeRecord   `json:"probes"`
	Circuits map[string]CircuitRecord `json:"circuits"`
}

func emptyState() State {
	return State{
		Vessels:  make(map[string]VesselRecord),
		Probes:   make(map[string]ProbeRecord),
		Circuits: make(map[string]CircuitRecord),
	}
}

// normalize repairs maps after decoding an empty or partial snapshot.
func (s State) normalize() State {
	if s.Vessels == nil {
		s.Vessels = make(map[string]VesselRecord)
	}
	if s.Probes == nil {
		s.Probes = make(map[string]ProbeRecord)
	}
	if s.Circuits == nil {
		s.Circuits = make(map[string]CircuitRecord)
	}
	return s
}

func (s State) clone() State {
	clone := emptyState()
	for id, record := range s.Vessels {
		clone.Vessels[id] = record
	}
	for id, record := range s.Probes {
		clone.Probes[id] = record
	}
	for id, record := range s.Circuits {
		clone.Circuits[id] = record
	}
	return clone
}

func (s State) empty() bool {
	return len(s.Vessels) == 0 && len(s.Probes) == 0 && len(s.Circuits) == 0
}

func (s State) idTaken(id string) bool {
	if _, ok := s.Vessels[id]; ok {
		return true
	}
	if _, ok := s.Probes[id]; ok {
		return true
	}
	if _, ok := s.Circuits[id]; ok {
		return true
	}
	return false
}

// validate checks the invariants that must hold for the whole catalog:
// well-formed records, globally unique device ids, probe bindings that
// target active vessels, and unique zone/valve assignments among active
// circuits.
func (s State) validate() error {
	owners := make(map[string]string)
	claim := func(id, kind string) error {
		if other, taken := owners[id]; taken {
			return fmt.Errorf("device id %q is used by both %s and %s", id, other, kind)
		}
		owners[id] = kind
		return nil
	}
	for id, record := range s.Vessels {
		if record.ID != id {
			return fmt.Errorf("vessel record key %q does not match id %q", id, record.ID)
		}
		if err := record.validate(); err != nil {
			return err
		}
		if err := claim(id, "vessel"); err != nil {
			return err
		}
	}
	for id, record := range s.Probes {
		if record.ID != id {
			return fmt.Errorf("probe record key %q does not match id %q", id, record.ID)
		}
		if err := record.validate(); err != nil {
			return err
		}
		if err := claim(id, "probe"); err != nil {
			return err
		}
		if !record.Active {
			continue
		}
		vessel, ok := s.Vessels[record.VesselID]
		if !ok {
			return fmt.Errorf("probe %q is bound to unregistered vessel %q", record.ID, record.VesselID)
		}
		if !vessel.Active {
			return fmt.Errorf("probe %q is bound to deactivated vessel %q", record.ID, record.VesselID)
		}
	}
	circuitZones := make(map[string]string)
	circuitValves := make(map[string]string)
	for id, record := range s.Circuits {
		if record.ID != id {
			return fmt.Errorf("circuit record key %q does not match id %q", id, record.ID)
		}
		if err := record.validate(); err != nil {
			return err
		}
		if err := claim(id, "circuit"); err != nil {
			return err
		}
		if !record.Active {
			continue
		}
		if other, taken := circuitZones[record.Zone]; taken {
			return fmt.Errorf("zone %q is served by both circuit %q and circuit %q", record.Zone, other, record.ID)
		}
		circuitZones[record.Zone] = record.ID
		if other, taken := circuitValves[record.ValveID]; taken {
			return fmt.Errorf("valve %q is assigned to both circuit %q and circuit %q", record.ValveID, other, record.ID)
		}
		circuitValves[record.ValveID] = record.ID
	}
	return nil
}

// View is the read model returned to API clients. Devices are sorted by
// id and include deactivated records.
type View struct {
	Vessels  []VesselRecord  `json:"vessels"`
	Probes   []ProbeRecord   `json:"probes"`
	Circuits []CircuitRecord `json:"circuits"`
}

func (s State) view() View {
	view := View{
		Vessels:  make([]VesselRecord, 0, len(s.Vessels)),
		Probes:   make([]ProbeRecord, 0, len(s.Probes)),
		Circuits: make([]CircuitRecord, 0, len(s.Circuits)),
	}
	for _, record := range s.Vessels {
		view.Vessels = append(view.Vessels, record)
	}
	for _, record := range s.Probes {
		view.Probes = append(view.Probes, record)
	}
	for _, record := range s.Circuits {
		view.Circuits = append(view.Circuits, record)
	}
	sort.Slice(view.Vessels, func(i, j int) bool { return view.Vessels[i].ID < view.Vessels[j].ID })
	sort.Slice(view.Probes, func(i, j int) bool { return view.Probes[i].ID < view.Probes[j].ID })
	sort.Slice(view.Circuits, func(i, j int) bool { return view.Circuits[i].ID < view.Circuits[j].ID })
	return view
}
