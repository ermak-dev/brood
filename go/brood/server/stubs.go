// Точка сборки Substrate-части: обработчики actor/template/worker/policy
// живут в api_actors.go (#32), резолвер адреса актора — в resolver.go.
// Здесь — только адаптер пользователя для proxy (#33).
package server

import (
	"context"

	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/proxy"
)

// proxyCurrentUser — адаптер proxy.CurrentUser над middleware auth:
// пользователь запроса уже аутентифицирован общим Middleware сервера,
// прокси аутентификацией не занимается.
func proxyCurrentUser(ctx context.Context) (proxy.Identity, bool) {
	user, ok := auth.UserFromContext(ctx)
	if !ok {
		return proxy.Identity{}, false
	}
	return proxy.Identity{ID: user.ID, Username: user.Username}, true
}
