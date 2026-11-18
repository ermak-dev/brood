package relay

import "context"

type sessionOpenGuardKey struct{}

// WithSessionOpenGuard installs a local fence at the actual SSH channel opening,
// including when a session adapter delays CallSession. The guard must return a
// release function on success. It is released after opening, before waiting for
// the guest's operation result. The guard is not serialized into the request.
func WithSessionOpenGuard(ctx context.Context, guard func(context.Context) (func(), error)) context.Context {
	return context.WithValue(ctx, sessionOpenGuardKey{}, guard)
}

func openSessionGuard(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if guard, ok := ctx.Value(sessionOpenGuardKey{}).(func(context.Context) (func(), error)); ok {
		return guard(ctx)
	}
	return func() {}, nil
}
