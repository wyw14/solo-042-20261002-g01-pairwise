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

func (c Circuit) Validate() error {
	if strings.TrimSpace(c.ID) == "" {
		return errors.New("circuit id is required")
	}
	if strings.TrimSpace(c.Zone) == "" {
		return errors.New("circuit zone is required")
	}
	if strings.TrimSpace(c.ValveID) == "" {
		return errors.New("circuit valve id is required")
	}
	return nil
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

func (r *Registry) Register(c Circuit) error {
	if err := c.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.circuits[c.ID]; exists {
		return errors.New("circuit already registered")
	}
	r.circuits[c.ID] = c
	return nil
}

func (r *Registry) Update(id string, apply func(Circuit) (Circuit, error)) (Circuit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.circuits[id]
	if !ok {
		return Circuit{}, errors.New("circuit not found")
	}
	next, err := apply(current)
	if err != nil {
		return Circuit{}, err
	}
	next.ID = current.ID
	if err := next.Validate(); err != nil {
		return Circuit{}, err
	}
	r.circuits[id] = next
	return next, nil
}

func (r *Registry) Remove(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.circuits[id]; !exists {
		return false
	}
	delete(r.circuits, id)
	return true
}

func (r *Registry) Get(id string) (Circuit, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	circuit, ok := r.circuits[id]
	return circuit, ok
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
