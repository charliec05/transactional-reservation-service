package reservation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrInsufficientStock   = errors.New("insufficient inventory")
	ErrIdempotencyConflict = errors.New("idempotency key reused with different parameters")
	ErrInvalidTransition   = errors.New("invalid reservation transition")
	ErrInvalidInput        = errors.New("invalid input")
)

type HoldStatus string

const (
	StatusHeld       HoldStatus = "held"
	StatusCheckedOut HoldStatus = "checked_out"
	StatusReleased   HoldStatus = "released"
	StatusExpired    HoldStatus = "expired"
)

type Resource struct {
	ID        string `json:"id"`
	Capacity  int    `json:"capacity"`
	Available int    `json:"available"`
}

type Hold struct {
	ID             string     `json:"id"`
	ResourceID     string     `json:"resource_id"`
	Quantity       int        `json:"quantity"`
	Status         HoldStatus `json:"status"`
	IdempotencyKey string     `json:"idempotency_key"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type Event struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	AggregateID string         `json:"aggregate_id"`
	OccurredAt  time.Time      `json:"occurred_at"`
	Attributes  map[string]any `json:"attributes,omitempty"`
}

type idempotencyRecord struct {
	ResourceID string `json:"resource_id"`
	Quantity   int    `json:"quantity"`
	HoldID     string `json:"hold_id"`
}

type persistedState struct {
	Resources   map[string]Resource          `json:"resources"`
	Holds       map[string]Hold              `json:"holds"`
	Idempotency map[string]idempotencyRecord `json:"idempotency"`
	Events      []Event                      `json:"events"`
	NextID      uint64                       `json:"next_id"`
}

type Store struct {
	mu        sync.Mutex
	state     persistedState
	stateFile string
	now       func() time.Time
}

type CreateHoldRequest struct {
	ResourceID     string
	Quantity       int
	TTL            time.Duration
	IdempotencyKey string
}

func NewStore(stateFile string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	s := &Store{stateFile: stateFile, now: now, state: emptyState()}
	if stateFile == "" {
		return s, nil
	}
	data, err := os.ReadFile(stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	s.ensureMaps()
	if err := checkState(s.state); err != nil {
		return nil, fmt.Errorf("invalid persisted state: %w", err)
	}
	return s, nil
}

func emptyState() persistedState {
	return persistedState{
		Resources:   make(map[string]Resource),
		Holds:       make(map[string]Hold),
		Idempotency: make(map[string]idempotencyRecord),
		Events:      make([]Event, 0),
	}
}

func (s *Store) ensureMaps() {
	if s.state.Resources == nil {
		s.state.Resources = make(map[string]Resource)
	}
	if s.state.Holds == nil {
		s.state.Holds = make(map[string]Hold)
	}
	if s.state.Idempotency == nil {
		s.state.Idempotency = make(map[string]idempotencyRecord)
	}
}

func cloneState(src persistedState) persistedState {
	dst := persistedState{
		Resources:   make(map[string]Resource, len(src.Resources)),
		Holds:       make(map[string]Hold, len(src.Holds)),
		Idempotency: make(map[string]idempotencyRecord, len(src.Idempotency)),
		Events:      append([]Event(nil), src.Events...),
		NextID:      src.NextID,
	}
	for key, value := range src.Resources {
		dst.Resources[key] = value
	}
	for key, value := range src.Holds {
		dst.Holds[key] = value
	}
	for key, value := range src.Idempotency {
		dst.Idempotency[key] = value
	}
	return dst
}

func (s *Store) AddResource(id string, capacity int) (Resource, error) {
	if id == "" || capacity <= 0 {
		return Resource{}, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.Resources[id]; exists {
		return Resource{}, fmt.Errorf("resource %q already exists: %w", id, ErrInvalidInput)
	}
	next := s.workingStateLocked()
	resource := Resource{ID: id, Capacity: capacity, Available: capacity}
	next.Resources[id] = resource
	appendEvent(next, "resource.created", id, s.now(), map[string]any{"capacity": capacity})
	if err := s.commitLocked(*next); err != nil {
		return Resource{}, err
	}
	return resource, nil
}

func (s *Store) CreateHold(request CreateHoldRequest) (Hold, bool, error) {
	if request.ResourceID == "" || request.Quantity <= 0 || request.TTL <= 0 || request.IdempotencyKey == "" {
		return Hold{}, false, ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now().UTC()
	next := s.workingStateLocked()
	expireState(next, now)
	if existing, ok := next.Idempotency[request.IdempotencyKey]; ok {
		if existing.ResourceID != request.ResourceID || existing.Quantity != request.Quantity {
			return Hold{}, false, ErrIdempotencyConflict
		}
		hold, ok := next.Holds[existing.HoldID]
		if !ok {
			return Hold{}, false, fmt.Errorf("idempotency record references missing hold: %w", ErrInvalidTransition)
		}
		if err := s.commitIfChangedLocked(*next); err != nil {
			return Hold{}, false, err
		}
		return hold, true, nil
	}

	resource, ok := next.Resources[request.ResourceID]
	if !ok {
		return Hold{}, false, ErrNotFound
	}
	if resource.Available < request.Quantity {
		return Hold{}, false, ErrInsufficientStock
	}

	resource.Available -= request.Quantity
	next.Resources[resource.ID] = resource
	next.NextID++
	hold := Hold{
		ID:             fmt.Sprintf("hold-%06d", next.NextID),
		ResourceID:     resource.ID,
		Quantity:       request.Quantity,
		Status:         StatusHeld,
		IdempotencyKey: request.IdempotencyKey,
		CreatedAt:      now,
		UpdatedAt:      now,
		ExpiresAt:      now.Add(request.TTL),
	}
	next.Holds[hold.ID] = hold
	next.Idempotency[request.IdempotencyKey] = idempotencyRecord{
		ResourceID: request.ResourceID,
		Quantity:   request.Quantity,
		HoldID:     hold.ID,
	}
	appendEvent(next, "hold.created", hold.ID, now, map[string]any{
		"resource_id": resource.ID,
		"quantity":    request.Quantity,
	})
	if err := s.commitLocked(*next); err != nil {
		return Hold{}, false, err
	}
	return hold, false, nil
}

func (s *Store) Checkout(id string) (Hold, error) {
	return s.transition(id, StatusCheckedOut)
}

func (s *Store) Release(id string) (Hold, error) {
	return s.transition(id, StatusReleased)
}

func (s *Store) transition(id string, target HoldStatus) (Hold, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	next := s.workingStateLocked()
	expireState(next, now)
	hold, ok := next.Holds[id]
	if !ok {
		return Hold{}, ErrNotFound
	}
	if hold.Status != StatusHeld {
		return Hold{}, fmt.Errorf("hold is %s: %w", hold.Status, ErrInvalidTransition)
	}
	if target != StatusCheckedOut && target != StatusReleased {
		return Hold{}, ErrInvalidTransition
	}
	if target == StatusReleased {
		resource := next.Resources[hold.ResourceID]
		resource.Available += hold.Quantity
		next.Resources[resource.ID] = resource
	}
	hold.Status = target
	hold.UpdatedAt = now
	next.Holds[id] = hold
	appendEvent(next, "hold."+string(target), id, now, nil)
	if err := s.commitLocked(*next); err != nil {
		return Hold{}, err
	}
	return hold, nil
}

func (s *Store) ExpireDue() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.workingStateLocked()
	count := expireState(next, s.now().UTC())
	if count == 0 {
		return 0, nil
	}
	if err := s.commitLocked(*next); err != nil {
		return 0, err
	}
	return count, nil
}

func expireState(state *persistedState, now time.Time) int {
	count := 0
	for id, hold := range state.Holds {
		if hold.Status != StatusHeld || hold.ExpiresAt.After(now) {
			continue
		}
		resource := state.Resources[hold.ResourceID]
		resource.Available += hold.Quantity
		state.Resources[resource.ID] = resource
		hold.Status = StatusExpired
		hold.UpdatedAt = now
		state.Holds[id] = hold
		appendEvent(state, "hold.expired", id, now, nil)
		count++
	}
	return count
}

func (s *Store) GetResource(id string) (Resource, error) {
	if _, err := s.ExpireDue(); err != nil {
		return Resource{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resource, ok := s.state.Resources[id]
	if !ok {
		return Resource{}, ErrNotFound
	}
	return resource, nil
}

func (s *Store) GetHold(id string) (Hold, error) {
	if _, err := s.ExpireDue(); err != nil {
		return Hold{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hold, ok := s.state.Holds[id]
	if !ok {
		return Hold{}, ErrNotFound
	}
	return hold, nil
}

func (s *Store) Events(after int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if after < 0 {
		after = 0
	}
	if after >= len(s.state.Events) {
		return []Event{}
	}
	return append([]Event(nil), s.state.Events[after:]...)
}

func (s *Store) CheckInvariants() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return checkState(s.state)
}

func checkState(state persistedState) error {
	committed := make(map[string]int)
	for _, hold := range state.Holds {
		if _, ok := state.Resources[hold.ResourceID]; !ok {
			return fmt.Errorf("hold %s references unknown resource %s", hold.ID, hold.ResourceID)
		}
		if hold.Status == StatusHeld || hold.Status == StatusCheckedOut {
			committed[hold.ResourceID] += hold.Quantity
		}
	}
	for id, resource := range state.Resources {
		if resource.Capacity <= 0 || resource.Available < 0 || resource.Available > resource.Capacity {
			return fmt.Errorf("resource %s has invalid inventory %d/%d", id, resource.Available, resource.Capacity)
		}
		if resource.Available+committed[id] != resource.Capacity {
			return fmt.Errorf("resource %s violates conservation: available=%d committed=%d capacity=%d", id, resource.Available, committed[id], resource.Capacity)
		}
	}
	return nil
}

func appendEvent(state *persistedState, eventType, aggregateID string, at time.Time, attributes map[string]any) {
	state.NextID++
	event := Event{
		ID:          fmt.Sprintf("event-%06d", state.NextID),
		Type:        eventType,
		AggregateID: aggregateID,
		OccurredAt:  at,
		Attributes:  attributes,
	}
	state.Events = append(state.Events, event)
}

func (s *Store) commitIfChangedLocked(next persistedState) error {
	if len(next.Events) == len(s.state.Events) {
		return nil
	}
	return s.commitLocked(next)
}

func (s *Store) workingStateLocked() *persistedState {
	if s.stateFile == "" {
		return &s.state
	}
	next := cloneState(s.state)
	return &next
}

func (s *Store) commitLocked(next persistedState) error {
	if err := checkState(next); err != nil {
		return err
	}
	if err := persistAtomically(s.stateFile, next); err != nil {
		return err
	}
	s.state = next
	return nil
}

func persistAtomically(path string, state persistedState) error {
	if path == "" {
		return nil
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".reservation-state-*")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		temporary.Close()
		return fmt.Errorf("encode state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	return nil
}

func (s *Store) Snapshot() ([]Resource, []Hold) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resources := make([]Resource, 0, len(s.state.Resources))
	holds := make([]Hold, 0, len(s.state.Holds))
	for _, resource := range s.state.Resources {
		resources = append(resources, resource)
	}
	for _, hold := range s.state.Holds {
		holds = append(holds, hold)
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].ID < resources[j].ID })
	sort.Slice(holds, func(i, j int) bool { return holds[i].ID < holds[j].ID })
	return resources, holds
}
