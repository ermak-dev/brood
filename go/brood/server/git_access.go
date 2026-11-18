package server

import (
	"context"
	"fmt"

	"brood.dev/go/brood/gitorigin"
	"brood.dev/go/brood/proxmox/egress"
)

// gitAccess — текущий серверный Git-доступ проекта (#173): repository и
// credential, который шлюз подставит в upstream. Источник — проектный Git
// credential, на который ссылается декларация и который привязан ровно к этому
// repository; иначе remote установки из BROOD_GIT_REMOTES (legacy). Значение
// пересчитывается на каждом чтении, старте и запросе шлюза.
type gitAccess struct {
	repository string
	credential gitCredential
	gateway    bool
	// bound — доступ дал проектный credential из ссылки декларации.
	bound bool
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

func (a gitAccess) modeWithNetwork(origin, network string) string {
	if network == "" {
		return a.mode(origin)
	}
	policy, err := egress.Decode([]byte(network))
	if err != nil {
		return ""
	}
	return gitorigin.Mode(origin, a.repository, func(string) bool { return a.gateway }, func(host string) bool { return policy.Allows(egress.TLS, host, 443) })
}

// projectGitAccess читает repository, ссылку декларации и привязанный
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
		access.gateway, access.bound = true, true
	} else if legacy, ok := s.git.remotes[stored.Repository]; ok && stored.Repository != "" {
		access.credential, access.gateway = legacy, true
	}
	return access, nil
}
