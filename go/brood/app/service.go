package app

import (
	"context"
	"sync"
	"time"
)

// Clock возвращает текущее время UTC; внедряется для тестов.
type Clock func() time.Time

// IDGen генерирует строковый идентификатор (ULID/UUID).
type IDGen func() string

// Service — прикладной слой: права и операции над проектами и роями.
type Service struct {
	store    Store
	now      Clock
	newID    IDGen
	projects sync.Map
}

// NewService собирает сервис поверх хранилища.
func NewService(store Store, now Clock, newID IDGen) *Service {
	return &Service{store: store, now: now, newID: newID}
}

func (s *Service) lockProjectContext(ctx context.Context, projectID string) (func(), error) {
	value, _ := s.projects.LoadOrStore(projectID, make(chan struct{}, 1))
	gate := value.(chan struct{})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	}
}

// lockProject serializes resource creation for an existing, authorized project.
// Keys are admitted only after validation, so arbitrary request IDs cannot grow it.
func (s *Service) lockProject(projectID string) func() {
	value, _ := s.projects.LoadOrStore(projectID, make(chan struct{}, 1))
	gate := value.(chan struct{})
	gate <- struct{}{}
	return func() { <-gate }
}
