//go:build linux

package guest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
	"brood.dev/go/brood/proxmox/guest/managedinbox"
	"brood.dev/go/brood/proxmox/relay"
)

// PreparedStepInbox is a root-validated loader profile. It does not change the
// active step policy or open a listener until used by the owning runtime.
type PreparedStepInbox struct {
	directory string
	fragment  []byte
}

// PrepareStepInbox requires explicitly provisioned, protected plugin state. It
// neither runs the pinned loader nor initializes an actor-controlled cache.
func PrepareStepInbox(ctx context.Context, directory string) (*PreparedStepInbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("supervisor inbox requires a private delivery directory")
	}
	publication, err := managedinbox.Publish(true)
	if err != nil {
		return nil, err
	}
	if err := managedinbox.VerifyProtectedPluginState(); err != nil {
		return nil, err
	}
	return &PreparedStepInbox{directory: directory, fragment: publication.ManagedSettings}, nil
}

// Start binds the separately prepared profile to this runtime and exact relay
// identity. The runtime writes it only during an attested held step launch.
func (p *PreparedStepInbox) Start(ctx context.Context, r *Runtime, cfg *relay.ClientConfig) (io.Closer, error) {
	if p == nil || p.directory == "" || len(p.fragment) == 0 || r == nil || cfg == nil || cfg.HandleInbox != nil || cfg.InboxReceiverID != "" {
		return nil, ErrInboxHandoffUnavailable
	}
	broker, err := r.StartStepInboxBroker(ctx, p.directory, deliverylog.Scope{ActorUID: cfg.ActorUID, Incarnation: cfg.Generation})
	if err != nil {
		return nil, err
	}
	policy := systemStepPolicy()
	profile := &stepInboxSessionPolicy{
		enter: func() error {
			if err := managedinbox.VerifyProtectedPluginState(); err != nil {
				return err
			}
			if err := policy.verifyInboxLoaderSources("/workspace/project"); err != nil {
				return err
			}
			return policy.writeManagedInbox(p.fragment)
		},
		authorize: policy.writeInboxAppParent,
		leave: func() error {
			return errors.Join(policy.removeInboxAppParent(), policy.writeManaged())
		},
	}
	r.stepMu.Lock()
	if r.inboxPolicy != nil || r.stopping() {
		err = ErrInboxHandoffUnavailable
	} else if err = broker.ConfigureRelay(cfg); err == nil {
		r.inboxPolicy = profile
	}
	r.stepMu.Unlock()
	if err != nil {
		_ = broker.Close()
		return nil, err
	}
	return broker, nil
}

// This opt-in profile is deliberately narrower than the ordinary session. It
// only uses the protected-state loader layout, not precedence over writable
// alternate plugin or managed policy sources. The root calls it in quiescence.
func (p stepPolicy) verifyInboxLoaderSources(project string) error {
	if !filepath.IsAbs(project) || filepath.Clean(project) != project {
		return errors.New("inbox launch requires an exact workspace path")
	}
	dir, err := p.managedDirectory(false)
	if err == nil {
		file := os.NewFile(uintptr(dir), "Claude managed settings directory")
		names, readErr := file.Readdirnames(2)
		_ = file.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) || len(names) > 1 || len(names) == 1 && names[0] != stepManagedFile {
			return errors.New("unexpected managed settings source for inbox launch")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	claudeHome := filepath.Join(p.Home, ".claude")
	if st, err := os.Lstat(claudeHome); (err == nil && !st.IsDir()) || (err != nil && !errors.Is(err, os.ErrNotExist)) {
		return errors.New("unsafe Claude home for inbox launch")
	}
	if err := p.verifyEmptyInboxPluginScaffold(claudeHome); err != nil {
		return err
	}
	for _, path := range []string{
		filepath.Join(p.Managed, "managed-settings.json"), filepath.Join(p.Managed, "plugins"),
		filepath.Join(claudeHome, "cowork_plugins"),
		filepath.Join(claudeHome, ".credentials.json"), filepath.Join(claudeHome, "settings.local.json"),
		filepath.Join(claudeHome, ".config.json"), filepath.Join(claudeHome, "remote-settings.json"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return errors.New("unproved alternate Claude loader source for inbox launch")
		}
	}
	for path := project; ; path = filepath.Dir(path) {
		if _, err := os.Lstat(filepath.Join(path, ".claude")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("unproved workspace policy source for inbox launch")
		}
		if path == "/" {
			return nil
		}
	}
}

func prepareInboxClaudeLaunch(l appLayout, warn io.Writer) ([]string, []string, error) {
	step, err := readStepSession(l.Run, l.RunUID, l.RunGID)
	if err != nil || !step {
		return nil, nil, errors.New("supervisor inbox app requires a protected step policy")
	}
	args, env, err := prepareClaudeLaunchMode(l, warn, true)
	if err != nil {
		return nil, nil, err
	}
	return args, append(env, "CLAUDE_CODE_PLUGIN_CACHE_DIR="+managedinbox.PluginStateDirectory), nil
}
