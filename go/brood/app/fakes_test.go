package app

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// memStore — внутрипроцессная реализация Store для тестов прикладного
// слоя. Транзакций нет: прикладной слой не опирается на них, настоящая
// реализация #29 выполняет свои инварианты.
type memStore struct {
	mu         sync.Mutex
	users      map[string]User
	projects   map[string]Project
	members    map[Membership]struct{}
	swarms     map[string]*Swarm
	swarmsByAt map[string]string
	grants     map[[2]string]string
}

func newMemStore() *memStore {
	return &memStore{
		users:      map[string]User{},
		projects:   map[string]Project{},
		members:    map[Membership]struct{}{},
		swarms:     map[string]*Swarm{},
		swarmsByAt: map[string]string{},
		grants:     map[[2]string]string{},
	}
}

func (s *memStore) CreateUser(u User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[u.ID]; ok {
		return errors.New("user exists")
	}
	s.users[u.ID] = u
	return nil
}

func (s *memStore) UserByID(id string) (User, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	return u, ok, nil
}

func (s *memStore) UpdateUser(u User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[u.ID]; !ok {
		return errors.New("user not found")
	}
	s.users[u.ID] = u
	return nil
}

func (s *memStore) CreateProject(p Project) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ex := range s.projects {
		if ex.Name == p.Name {
			return errors.New("project name exists")
		}
	}
	if _, ok := s.projects[p.ID]; ok {
		return errors.New("project exists")
	}
	s.projects[p.ID] = p
	return nil
}

func (s *memStore) ProjectByID(id string) (Project, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.projects[id]
	return p, ok, nil
}

func (s *memStore) Projects() ([]Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Project, 0, len(s.projects))
	for _, p := range s.projects {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *memStore) ProjectsForUser(userID string) ([]Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Project
	for m := range s.members {
		if m.UserID == userID {
			if p, ok := s.projects[m.ProjectID]; ok {
				out = append(out, p)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *memStore) AddMembership(m Membership) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.members[m]; ok {
		return errors.New("membership exists")
	}
	s.members[m] = struct{}{}
	return nil
}

func (s *memStore) RemoveMembership(projectID, userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := Membership{ProjectID: projectID, UserID: userID}
	if _, ok := s.members[m]; !ok {
		return errors.New("membership not found")
	}
	delete(s.members, m)
	return nil
}

func (s *memStore) IsMember(projectID, userID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.members[Membership{ProjectID: projectID, UserID: userID}]
	return ok, nil
}

func (s *memStore) MemberIDs(projectID string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for m := range s.members {
		if m.ProjectID == projectID {
			ids = append(ids, m.UserID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *memStore) SaveSwarm(sw Swarm) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.swarms[sw.ID]; ok {
		return errors.New("swarm exists")
	}
	if _, ok := s.swarmsByAt[sw.AtespaceName]; ok {
		return errors.New("atespace name taken")
	}
	cp := sw
	s.swarms[sw.ID] = &cp
	s.swarmsByAt[sw.AtespaceName] = sw.ID
	s.grants[[2]string{sw.ID, sw.OwnerID}] = RoleOwner
	return nil
}

func (s *memStore) SwarmByID(id string) (Swarm, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sw, ok := s.swarms[id]
	if !ok {
		return Swarm{}, false, nil
	}
	return *sw, true, nil
}

func (s *memStore) SwarmByAtespaceName(name string) (Swarm, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.swarmsByAt[name]
	if !ok {
		return Swarm{}, false, nil
	}
	return *s.swarms[id], true, nil
}

func (s *memStore) UpdateSwarm(sw Swarm) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.swarms[sw.ID]
	if !ok {
		return errors.New("swarm not found")
	}
	if cur.OwnerID != sw.OwnerID || cur.ProjectID != sw.ProjectID || cur.AtespaceName != sw.AtespaceName {
		return fmt.Errorf("%w: swarm identity changed", ErrConflict)
	}
	cur.State, cur.AtespaceUID, cur.Retry = sw.State, sw.AtespaceUID, sw.Retry
	cur.UpdatedAt = sw.UpdatedAt
	return nil
}

func (s *memStore) ChangeSwarmOwner(actorID, id, expectedOwnerID, newOwnerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.aclWriteAllowedLocked(actorID, id)
	if err != nil {
		return err
	}
	if cur.OwnerID != expectedOwnerID {
		return ErrConflict
	}
	if !s.validGranteeLocked(cur.ProjectID, newOwnerID) {
		return ErrNotAllowed
	}
	if err := s.changeGrantLocked(cur, newOwnerID, RoleOwner); err != nil {
		return err
	}
	if expectedOwnerID != newOwnerID {
		if err := s.changeGrantLocked(cur, expectedOwnerID, ""); err != nil {
			return err
		}
	}
	cur.OwnerID = newOwnerID
	return nil
}

func (s *memStore) SwarmsByProject(projectID string) ([]Swarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Swarm
	for _, sw := range s.swarms {
		if sw.ProjectID == projectID {
			out = append(out, *sw)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (s *memStore) SwarmsByOwner(projectID, ownerID string) ([]Swarm, error) {
	all, err := s.SwarmsByProject(projectID)
	if err != nil {
		return nil, err
	}
	var out []Swarm
	for _, sw := range all {
		if sw.OwnerID == ownerID {
			out = append(out, sw)
		}
	}
	return out, nil
}

func (s *memStore) Swarms() ([]Swarm, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Swarm
	for _, sw := range s.swarms {
		out = append(out, *sw)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// Compile-time check: memStore implements Store.
var _ Store = (*memStore)(nil)

// fakeProvisioner — заменитель Substrate-адаптера #32. Хранит созданные
// atespace'ы и умеет отказывать по сценарию теста.
type fakeProvisioner struct {
	mu         sync.Mutex
	byName     map[string]string // name -> uid
	next       int
	failGet    bool
	getErr     error
	failCreate bool
	createErr  error
	failDelete bool
	deleteErr  error
}

func newFakeProvisioner() *fakeProvisioner {
	return &fakeProvisioner{byName: map[string]string{}}
}

func (f *fakeProvisioner) CreateAtespace(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCreate {
		return "", f.createErr
	}
	if uid, ok := f.byName[name]; ok {
		return uid, nil
	}
	f.next++
	uid := fmt.Sprintf("uid-%d", f.next)
	f.byName[name] = uid
	return uid, nil
}

func (f *fakeProvisioner) GetAtespace(name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGet {
		return "", f.getErr
	}
	uid, ok := f.byName[name]
	if !ok {
		return "", ErrNotFound
	}
	return uid, nil
}

func (f *fakeProvisioner) DeleteAtespace(name, uid string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete {
		return f.deleteErr
	}
	cur, ok := f.byName[name]
	if !ok {
		return nil
	}
	if cur != uid {
		return ErrConflict
	}
	delete(f.byName, name)
	return nil
}

// Compile-time check: fakeProvisioner implements Provisioner.
var _ Provisioner = (*fakeProvisioner)(nil)
