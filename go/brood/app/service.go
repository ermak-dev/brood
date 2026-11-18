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

// Provisioner — граница с Substrate (#32): создание и удаление atespace.
// Реальный адаптер проверяет UID при удалении; фейк в тестах повторяет
// то же правило.
type Provisioner interface {
	// CreateAtespace создаёт atespace и возвращает его UID.
	CreateAtespace(name string) (uid string, err error)
	// GetAtespace возвращает UID существующего atespace.
	GetAtespace(name string) (uid string, err error)
	// DeleteAtespace удаляет atespace по имени и UID.
	DeleteAtespace(name, uid string) error
}

// Service — прикладной слой проектов, членства и роёв.
// Молчит: данные и ошибки возвращает, печатает только внешний слой.
type Service struct {
	store       Store
	provisioner Provisioner
	now         Clock
	newID       IDGen
	projects    sync.Map
}

// NewService собирает сервис из зависимостей.
func NewService(store Store, provisioner Provisioner, now Clock, newID IDGen) *Service {
	return &Service{store: store, provisioner: provisioner, now: now, newID: newID}
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
