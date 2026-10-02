package catalog

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/wyw14/cry-98/internal/model"
)

// DeviceKind identifies which hardware family a catalog entry belongs to.
type DeviceKind string

const (
	KindVessel  DeviceKind = "vessel"
	KindProbe   DeviceKind = "probe"
	KindCircuit DeviceKind = "circuit"
)

func (k DeviceKind) valid() bool {
	switch k {
	case KindVessel, KindProbe, KindCircuit:
		return true
	default:
		return false
	}
}

// DeviceStatus records whether a catalog entry is in service.
type DeviceStatus string

const (
	StatusActive      DeviceStatus = "active"
	StatusDeactivated DeviceStatus = "deactivated"
)

// Device is the catalog representation of one vessel, probe, or circuit.
// Only fields relevant to the device's kind are populated.
type Device struct {
	ID        string          `json:"id"`
	Kind      DeviceKind      `json:"type"`
	Status    DeviceStatus    `json:"status"`
	Name      string          `json:"name,omitempty"`
	Zone      string          `json:"zone,omitempty"`
	ProbeKind model.ProbeKind `json:"probe_kind,omitempty"`
	VesselID  string          `json:"vessel_id,omitempty"`
	ValveID   string          `json:"valve_id,omitempty"`
	Enabled   *bool           `json:"enabled,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func (d Device) active() bool {
	return d.Status != StatusDeactivated
}

// Config is the complete, persisted device catalog. It is validated as a
// single unit: every binding and uniqueness constraint is checked across
// all entries before any change is applied to the running registries.
type Config struct {
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	Devices   []Device  `json:"devices"`
}

// DeviceRequest is the API payload for registering or amending a device.
// The Enabled pointer lets callers distinguish "unchanged" from false.
type DeviceRequest struct {
	ID        string          `json:"id"`
	Kind      DeviceKind      `json:"type"`
	Name      string          `json:"name"`
	Zone      string          `json:"zone"`
	ProbeKind model.ProbeKind `json:"probe_kind"`
	VesselID  string          `json:"vessel_id"`
	ValveID   string          `json:"valve_id"`
	Enabled   *bool           `json:"enabled"`
}

// Sentinel errors used to map catalog failures onto HTTP responses.
var (
	ErrDeviceNotFound    = errors.New("device not found")
	ErrDeviceDeactivated = errors.New("device is deactivated")
	ErrDeviceOccupied    = errors.New("device is occupied by an active fill")
)

func trim(v string) string {
	return strings.TrimSpace(v)
}

// validateConfig verifies the whole catalog atomically and returns every
// problem found so callers can reject a configuration without applying any
// part of it.
func validateConfig(config Config) error {
	var problems []string
	addProblem := func(message string) { problems = append(problems, message) }

	kindByID := make(map[string]DeviceKind)
	activeVessels := make(map[string]bool)
	valveOwner := make(map[string]string)

	for index, device := range config.Devices {
		label := device.ID
		if trim(label) == "" {
			label = "device[" + strconv.Itoa(index) + "]"
		}
		id := trim(device.ID)
		if id == "" {
			addProblem("device id is required")
			continue
		}
		if previous, dup := kindByID[id]; dup {
			addProblem("duplicate device id " + id + " (already present as " + string(previous) + ")")
			continue
		}
		if !device.Kind.valid() {
			addProblem(label + ": unknown device type " + string(device.Kind))
			continue
		}
		if device.Status != StatusActive && device.Status != StatusDeactivated {
			addProblem(label + ": unknown device status " + string(device.Status))
			continue
		}
		kindByID[id] = device.Kind
		if !device.active() {
			continue
		}
		switch device.Kind {
		case KindVessel:
			if trim(device.Name) == "" {
				addProblem(label + ": vessel name is required")
			}
			if trim(device.Zone) == "" {
				addProblem(label + ": vessel zone is required")
			}
			activeVessels[id] = true
		case KindProbe:
			if !device.ProbeKind.Valid() {
				addProblem(label + ": unknown probe kind")
			}
			if trim(device.VesselID) == "" {
				addProblem(label + ": probe binding vessel_id is required")
			}
			if trim(device.VesselID) == id {
				addProblem(label + ": probe cannot bind to itself")
			}
		case KindCircuit:
			if trim(device.Zone) == "" {
				addProblem(label + ": circuit zone is required")
			}
			valve := trim(device.ValveID)
			if valve == "" {
				addProblem(label + ": circuit valve_id is required")
				continue
			}
			if holder, taken := valveOwner[valve]; taken {
				addProblem(label + ": valve " + valve + " is already assigned to circuit " + holder)
				continue
			}
			valveOwner[valve] = id
		}
	}

	// Cross-entry checks are a separate pass so slice ordering does not
	// matter: every active probe must bind an active vessel.
	for _, device := range config.Devices {
		if !device.active() || device.Kind != KindProbe {
			continue
		}
		target := trim(device.VesselID)
		if target == "" || target == device.ID {
			continue
		}
		kind, known := kindByID[target]
		switch {
		case !known:
			addProblem(device.ID + ": bound vessel " + target + " is not registered")
		case kind != KindVessel:
			addProblem(device.ID + ": binding target " + target + " is not a vessel")
		case !activeVessels[target]:
			addProblem(device.ID + ": bound vessel " + target + " is deactivated")
		}
	}

	if len(problems) == 0 {
		return nil
	}
	errs := make([]error, 0, len(problems))
	for _, message := range problems {
		errs = append(errs, errors.New(message))
	}
	return errors.Join(errs...)
}
