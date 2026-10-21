package controller

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/valthaide/conops-private/internal/api"
)

const (
	OpPending   = "pending"
	OpRunning   = "running"
	OpSucceeded = "succeeded"
	OpFailed    = "failed"
	OpKindReconcile = "reconcile"
)

// OperationStore keeps in-memory async operations for this process.
type OperationStore struct {
	mu      sync.Mutex
	byID    map[string]*api.Operation
	byAppID map[string]string // appID -> active (pending/running) operation id
}

// NewOperationStore creates an empty operation store.
func NewOperationStore() *OperationStore {
	return &OperationStore{
		byID:    make(map[string]*api.Operation),
		byAppID: make(map[string]string),
	}
}

// Get returns a copy of an operation by id.
func (s *OperationStore) Get(id string) (*api.Operation, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.byID[id]
	if !ok || op == nil {
		return nil, false
	}
	cp := *op
	return &cp, true
}

// ActiveForApp returns the pending/running operation for an app, if any.
func (s *OperationStore) ActiveForApp(appID string) (*api.Operation, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byAppID[appID]
	if !ok {
		return nil, false
	}
	op, ok := s.byID[id]
	if !ok || op == nil {
		delete(s.byAppID, appID)
		return nil, false
	}
	if op.Status != OpPending && op.Status != OpRunning {
		delete(s.byAppID, appID)
		return nil, false
	}
	cp := *op
	return &cp, true
}

// StartReconcile creates a pending reconcile operation (or returns the active one).
func (s *OperationStore) StartReconcile(appID, appName string) (op *api.Operation, existing bool) {
	if s == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, ok := s.byAppID[appID]; ok {
		if cur, found := s.byID[id]; found && cur != nil && (cur.Status == OpPending || cur.Status == OpRunning) {
			cp := *cur
			return &cp, true
		}
		delete(s.byAppID, appID)
	}
	now := time.Now().UTC()
	created := &api.Operation{
		ID:        uuid.NewString(),
		AppID:     appID,
		AppName:   appName,
		Kind:      OpKindReconcile,
		Status:    OpPending,
		CreatedAt: now,
	}
	s.byID[created.ID] = created
	s.byAppID[appID] = created.ID
	cp := *created
	return &cp, false
}

// MarkRunning transitions pending → running.
func (s *OperationStore) MarkRunning(id string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.byID[id]
	if !ok || op == nil {
		return
	}
	op.Status = OpRunning
	op.StartedAt = time.Now().UTC()
}

// Finish marks an operation terminal and clears the active app slot.
func (s *OperationStore) Finish(id, status, code, stage, message string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.byID[id]
	if !ok || op == nil {
		return
	}
	op.Status = status
	op.Code = code
	op.Stage = stage
	op.Message = message
	op.FinishedAt = time.Now().UTC()
	if cur, ok := s.byAppID[op.AppID]; ok && cur == id {
		delete(s.byAppID, op.AppID)
	}
}
