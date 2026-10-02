package circuit

import (
	"errors"
	"sort"
	"strings"
	"sync"
)

type Circuit struct {
	ID       string  `json:"id"`
	Zone     string  `json:"zone"`
	ValveID  string  `json:"valve_id"`
	Enabled  bool    `json:"enabled"`
	Pressure float64 `json:"pressure_kpa"`
}

type Registry struct {
	mu       sync.RWMutex
	circuits map[string]Circuit
}

func NewRegistry(circuits ...Circuit) *Registry {
	r := &Registry{circuits: make(map[string]Circuit)}
	for _, circuit := range circuits {
		if circuit.ID != "" {
			r.circuits[circuit.ID] = circuit
		}
	}
	return r
}

func (r *Registry) Get(id string) (Circuit, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	circuit, ok := r.circuits[id]
	return circuit, ok
}

func (r *Registry) Register(circuit Circuit) error {
	if strings.TrimSpace(circuit.ID) == "" {
		return errors.New("circuit id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.circuits[circuit.ID]; exists {
		return errors.New("circuit already registered")
	}
	r.circuits[circuit.ID] = circuit
	return nil
}

func (r *Registry) Remove(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.circuits[id]; !ok {
		return errors.New("circuit not found")
	}
	delete(r.circuits, id)
	return nil
}

func (r *Registry) UpdateConfig(id, zone, valveID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	circuit, ok := r.circuits[id]
	if !ok {
		return errors.New("circuit not found")
	}
	circuit.Zone = zone
	circuit.ValveID = valveID
	r.circuits[id] = circuit
	return nil
}

func (r *Registry) List() []Circuit {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]Circuit, 0, len(r.circuits))
	for _, circuit := range r.circuits {
		result = append(result, circuit)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (r *Registry) UpdatePressure(id string, pressure float64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	circuit, ok := r.circuits[id]
	if !ok {
		return errors.New("circuit not found")
	}
	circuit.Pressure = pressure
	r.circuits[id] = circuit
	return nil
}

func (r *Registry) SetEnabled(id string, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	circuit, ok := r.circuits[id]
	if !ok {
		return errors.New("circuit not found")
	}
	circuit.Enabled = enabled
	r.circuits[id] = circuit
	return nil
}
