package server

import (
	"context"
	"fmt"

	"brood.dev/go/brood/gitorigin"
)

// gitAccess — текущий серверный Git-доступ проекта (#173): repository и
// credential, который шлюз подставит в upstream. Источник — проектный Git
// credential, выбранный для проекта и который привязан ровно к этому
// repository; иначе remote установки из BROOD_GIT_REMOTES (legacy). Значение
// пересчитывается на каждом чтении, старте и запросе шлюза.
type gitAccess struct {
	repository string
	credential gitCredential
	gateway    bool
}

// fmt печатает неэкспортируемые поля без их String: скрываем всю структуру.
func (gitAccess) String() string               { return "server.gitAccess{redacted}" }
func (a gitAccess) GoString() string           { return a.String() }
func (a gitAccess) Format(f fmt.State, _ rune) { redactFormat(f, a) }

// mode классифицирует origin актора: gateway — только repository проекта при
// настроенном доступе, direct — публичный host политики гостя.
func (a gitAccess) mode(origin string) string {
	if origin == "" {
		return ""
	}
	return gitorigin.Mode(origin, a.repository, func(string) bool { return a.gateway }, managedPublicHost)
}

// projectGitAccess читает repository, настройку проекта и привязанный
// credential одним снимком. Проектный credential вытесняет legacy remote того
// же repository: его ротация и удаление не требуют перезапуска сервера.
func (s *Server) projectGitAccess(ctx context.Context, projectID string) (gitAccess, error) {
	stored, err := s.store.ProjectGitAccess(ctx, projectID)
	if err != nil {
		return gitAccess{}, err
	}
	access := gitAccess{repository: stored.Repository}
	if stored.HasCredential() {
		access.credential = gitCredential{username: stored.Credential.Username, token: stored.Credential.Token}
		access.gateway = true
	} else if legacy, ok := s.git.remotes[stored.Repository]; ok && stored.Repository != "" {
		access.credential, access.gateway = legacy, true
	}
	return access, nil
}
