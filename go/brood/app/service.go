package app

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"brood.dev/go/brood/store"
)

// Service applies the access rules and the operations on projects,
// membership and swarms to the store, with the caller's context.
type Service struct {
	store *store.Store
	// projects — затвор каждого проекта: создание роя, смена лимитов и
	// конфигурации и допуск агента в одном проекте идут по очереди.
	projects sync.Map
}

// NewService builds the service over the store.
func NewService(st *store.Store) *Service {
	return &Service{store: st}
}

// lockProject занимает затвор проекта до unlock или отмены ctx. Затвор
// заводится только для уже проверенного проекта: произвольные ID из
// запросов карту не растят. Внутри транзакции store его не берут: та держит
// единственное соединение базы.
func (s *Service) lockProject(ctx context.Context, projectID string) (func(), error) {
	value, _ := s.projects.LoadOrStore(projectID, make(chan struct{}, 1))
	gate := value.(chan struct{})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	}
}

// activeUser читает пользователя; отсутствующий и заблокированный — отказ.
func (s *Service) activeUser(ctx context.Context, userID string) (store.User, error) {
	user, err := s.store.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && user.Disabled) {
		return store.User{}, ErrNotAllowed
	}
	return user, err
}

// requireAdmin пропускает только активного администратора.
func (s *Service) requireAdmin(ctx context.Context, userID string) error {
	user, err := s.activeUser(ctx, userID)
	if err != nil {
		return err
	}
	if !user.IsAdmin {
		return ErrNotAllowed
	}
	return nil
}

// projectExists отличает отсутствующий проект от запрета.
func (s *Service) projectExists(ctx context.Context, projectID string) error {
	_, err := s.store.GetProject(ctx, projectID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: project not found", ErrNotFound)
	}
	return err
}
