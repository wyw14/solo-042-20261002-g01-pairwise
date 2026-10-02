package telemetry

import (
	"sort"
	"sync"
	"time"

	"github.com/wyw14/cry-98/internal/model"
)

type Snapshot struct {
	Vessels   map[string]model.Vessel `json:"vessels"`
	UpdatedAt time.Time               `json:"updated_at"`
}

type SnapshotStore struct {
	mu      sync.RWMutex
	vessels map[string]model.Vessel
	updated time.Time
}

func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{vessels: make(map[string]model.Vessel)}
}

func (s *SnapshotStore) Apply(vessel model.Vessel) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vessels[vessel.ID] = vessel
	s.updated = vessel.UpdatedAt
}

func (s *SnapshotStore) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.vessels, id)
}

func (s *SnapshotStore) Get(id string) (model.Vessel, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	vessel, ok := s.vessels[id]
	return vessel, ok
}

func (s *SnapshotStore) StableSnapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	copyMap := make(map[string]model.Vessel, len(s.vessels))
	for id, vessel := range s.vessels {
		copyMap[id] = vessel
	}
	return Snapshot{Vessels: copyMap, UpdatedAt: s.updated}
}

func (s *SnapshotStore) List() []model.Vessel {
	snapshot := s.StableSnapshot()
	result := make([]model.Vessel, 0, len(snapshot.Vessels))
	for _, vessel := range snapshot.Vessels {
		result = append(result, vessel)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (s *SnapshotStore) Replace(vessels []model.Vessel, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vessels = make(map[string]model.Vessel, len(vessels))
	for _, vessel := range vessels {
		s.vessels[vessel.ID] = vessel
	}
	s.updated = now.UTC()
}
