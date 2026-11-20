package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/sshboot"
	"brood.dev/go/brood/store"
)

// Образ проекта. Сервер создаёт сборочный контейнер из базового шаблона
// пула, клонирует в нём репозиторий проекта на заданном коммите, играет
// плейбук Ansible локально, останавливает контейнер и превращает его в
// шаблон PVE. Воркеры проекта в этом пуле — клоны последнего готового
// образа. Сборка — одна последовательная задача со своим сроком; сборку,
// которую оборвала остановка сервера, старт помечает неуспешной.

const (
	imageBuildTimeout = 60 * time.Minute
	imageLogLimit     = 64 << 10
)

var (
	imageRef      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imagePlaybook = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_./-]*\.ya?ml$`)
)

type imageJSON struct {
	ID        string `json:"id"`
	Pool      string `json:"pool"`
	State     string `json:"state"`
	Ref       string `json:"ref"`
	Playbook  string `json:"playbook"`
	CTID      int    `json:"ctid"`
	Error     string `json:"error"`
	Log       string `json:"log"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func (s *Server) mountProjectImages() {
	s.mux.HandleFunc("GET /api/projects/{id}/images", s.handleProjectImages)
	s.mux.HandleFunc("POST /api/projects/{id}/images", s.handleProjectImages)
	s.mux.HandleFunc("GET /api/projects/{id}/images/{image}", s.handleProjectImage)
	s.mux.HandleFunc("DELETE /api/projects/{id}/images/{image}", s.handleProjectImage)
}

func (s *Server) imageJSON(ctx context.Context, i store.ProjectImage) imageJSON {
	pool := ""
	if p, err := s.store.GetManagedPool(ctx, i.PoolUID); err == nil {
		pool = p.Name
	}
	return imageJSON{ID: i.ID, Pool: pool, State: i.State, Ref: i.Ref, Playbook: i.Playbook, CTID: i.CTID, Error: i.Error, Log: i.Log, CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt}
}

// imageProject пропускает участника проекта и администратора; чужой проект
// скрыт как 404.
func (s *Server) imageProject(w http.ResponseWriter, r *http.Request) (store.User, string, bool) {
	user, ok := currentUser(w, r)
	if !ok {
		return user, "", false
	}
	id := r.PathValue("id")
	allowed, err := s.app.Can(user.ID, app.ActionViewProject, app.ProjectRes(id))
	if err != nil {
		apiError(w, r, err)
		return user, "", false
	}
	if !allowed {
		writeDetail(w, http.StatusNotFound, "project not found")
		return user, "", false
	}
	return user, id, true
}

func (s *Server) handleProjectImages(w http.ResponseWriter, r *http.Request) {
	_, projectID, ok := s.imageProject(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		rows, err := s.store.ListProjectImages(r.Context(), projectID)
		if err != nil {
			apiError(w, r, err)
			return
		}
		items := make([]imageJSON, 0, len(rows))
		for _, row := range rows {
			items = append(items, s.imageJSON(r.Context(), row))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	var body struct {
		Ref      string `json:"ref"`
		Playbook string `json:"playbook"`
		Pool     string `json:"pool"`
	}
	if !decodeAccessBody(w, r, &body) {
		return
	}
	if !imageRef.MatchString(body.Ref) {
		writeDetail(w, http.StatusBadRequest, "ref must be a full 40-character commit")
		return
	}
	if !imagePlaybook.MatchString(body.Playbook) || path.Clean(body.Playbook) != body.Playbook || strings.Contains(body.Playbook, "..") || len(body.Playbook) > 255 {
		writeDetail(w, http.StatusBadRequest, "playbook must be a relative .yml or .yaml path inside the repository")
		return
	}
	image, err := s.managed.startImageBuild(r.Context(), projectID, body.Pool, body.Ref, body.Playbook)
	if err != nil {
		var refusal *imageRefusal
		switch {
		case errors.As(err, &refusal):
			writeDetail(w, http.StatusConflict, refusal.detail)
		case errors.Is(err, store.ErrProjectImageBusy):
			writeDetail(w, http.StatusConflict, err.Error())
		default:
			managedAPIError(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusAccepted, s.imageJSON(r.Context(), image))
}

func (s *Server) handleProjectImage(w http.ResponseWriter, r *http.Request) {
	_, projectID, ok := s.imageProject(w, r)
	if !ok {
		return
	}
	image, err := s.store.GetProjectImage(r.Context(), projectID, r.PathValue("image"))
	if err != nil {
		apiError(w, r, err)
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, s.imageJSON(r.Context(), image))
		return
	}
	if err := s.managed.deleteImage(r.Context(), image); err != nil {
		var refusal *imageRefusal
		if errors.As(err, &refusal) {
			writeDetail(w, http.StatusConflict, refusal.detail)
			return
		}
		managedAPIError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// imageRefusal — отказ с причиной для вызывающего (409).
type imageRefusal struct{ detail string }

func (e *imageRefusal) Error() string { return e.detail }

func imageOwned(i store.ProjectImage) proxmox.OwnedCT {
	return proxmox.OwnedCT{VMID: i.CTID, Owner: proxmox.Ownership{PoolUID: i.PoolUID, ActorUID: i.ID, Incarnation: 1, Nonce: i.OwnerNonce}}
}

// imagePool выбирает пул сборки: названный или единственный включённый.
func (m *managedProvider) imagePool(ctx context.Context, name string) (store.ManagedPool, error) {
	if name != "" {
		p, err := m.poolByName(ctx, name)
		if err != nil {
			return p, &imageRefusal{detail: "pool " + name + " is not an enabled Proxmox pool"}
		}
		return p, nil
	}
	pools, err := m.pools(ctx)
	if err != nil {
		return store.ManagedPool{}, err
	}
	var enabled []store.ManagedPool
	for _, p := range pools {
		if p.State == "enabled" && m.poolAvailable(p) {
			enabled = append(enabled, p)
		}
	}
	if len(enabled) != 1 {
		return store.ManagedPool{}, &imageRefusal{detail: fmt.Sprintf("name the pool: %d enabled pools", len(enabled))}
	}
	return enabled[0], nil
}

// startImageBuild проверяет вход, резервирует контейнер и запускает сборку
// в фоне. Запись сборки фиксируется до первой записи в PVE.
func (m *managedProvider) startImageBuild(ctx context.Context, projectID, poolName, ref, playbook string) (store.ProjectImage, error) {
	if m.identity == nil || m.buildContext == nil {
		return store.ProjectImage{}, &imageRefusal{detail: "image builds are unavailable: the installation SSH identity is missing"}
	}
	binary, err := os.ReadFile(filepath.Join(m.server.artifactsDir, "brood-guest"))
	if err != nil || m.server.artifactsDir == "" {
		return store.ProjectImage{}, &imageRefusal{detail: "image builds are unavailable: the agent binary is not published"}
	}
	access, err := m.server.projectGitAccess(ctx, projectID)
	if err != nil {
		return store.ProjectImage{}, err
	}
	if access.repository == "" {
		return store.ProjectImage{}, &imageRefusal{detail: "the project has no repository; declare it in the Broodfile and run bro build"}
	}
	pool, err := m.imagePool(ctx, poolName)
	if err != nil {
		return store.ProjectImage{}, err
	}
	if pool.State != "enabled" || !m.poolAvailable(pool) {
		return store.ProjectImage{}, &imageRefusal{detail: "the pool is not enabled"}
	}
	resources, err := managedActorResources(pool)
	if err != nil {
		return store.ProjectImage{}, err
	}
	m.lifecycle.Lock()
	client, err := m.client(pool)
	if err != nil {
		m.lifecycle.Unlock()
		return store.ProjectImage{}, err
	}
	id, err := client.NextID(ctx)
	if err != nil {
		m.lifecycle.Unlock()
		return store.ProjectImage{}, err
	}
	image, err := m.server.store.CreateProjectImage(ctx, store.ProjectImage{ID: store.NewID(), ProjectID: projectID, PoolUID: pool.UID, CTID: id, OwnerNonce: store.NewID(), Ref: ref, Playbook: playbook})
	if err != nil {
		m.lifecycle.Unlock()
		return store.ProjectImage{}, err
	}
	spec := proxmox.CTSpec{VMID: image.CTID, Hostname: "brood-image-" + image.ID, Template: pool.Template.URI, Owner: imageOwned(image).Owner, Cores: int(resources.CPUMillicores / 1000), MemoryMiB: int(resources.MemoryBytes / mib), DiskGiB: int(resources.DiskBytes / gib), Nesting: true, SSHPublicKeys: m.identity.AuthorizedKey()}
	upid, err := client.Create(ctx, spec)
	m.lifecycle.Unlock()
	if err != nil {
		_ = m.server.store.FinishProjectImage(context.WithoutCancel(ctx), image.ID, "failed", "the build container was not created: "+managedProviderError(err))
		return store.ProjectImage{}, &imageRefusal{detail: "the build container was not created: " + managedProviderError(err)}
	}
	build := &imageBuild{provider: m, client: client, image: image, access: access, binary: binary}
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		build.run(m.buildContext, upid)
	}()
	return image, nil
}

// deleteImage уничтожает контейнер образа и снимает запись. Идущую сборку
// и образ с живыми клонами удалить нельзя.
func (m *managedProvider) deleteImage(ctx context.Context, image store.ProjectImage) error {
	if image.State == "building" {
		return &imageRefusal{detail: "the image is still building"}
	}
	used, err := m.server.store.ProjectImageInUse(ctx, image.ID)
	if err != nil {
		return err
	}
	if used {
		return &imageRefusal{detail: "workers cloned from this image still exist; delete them first"}
	}
	pool, err := m.server.store.GetManagedPool(ctx, image.PoolUID)
	if err != nil {
		return err
	}
	client, err := m.client(pool)
	if err != nil {
		return err
	}
	if err := destroyImageContainer(ctx, client, image); err != nil {
		return &imageRefusal{detail: "the image container was not removed: " + managedProviderError(err)}
	}
	return m.server.store.DeleteProjectImage(ctx, image.ID)
}

// destroyImageContainer останавливает и удаляет контейнер образа; уже
// отсутствующий контейнер — успех.
func destroyImageContainer(ctx context.Context, client proxmoxAPI, image store.ProjectImage) error {
	owned := imageOwned(image)
	if client.ConfirmAbsent(ctx, image.CTID) == nil {
		return nil
	}
	if _, err := client.InspectOwned(ctx, owned); err != nil {
		return err
	}
	if status, err := client.Status(ctx, image.CTID); err == nil && status.Status == "running" {
		upid, err := client.Stop(ctx, owned)
		if err != nil {
			return err
		}
		if err := client.WaitTask(ctx, upid); err != nil {
			return err
		}
	}
	upid, err := client.Delete(ctx, owned)
	if err != nil {
		return err
	}
	return client.WaitTask(ctx, upid)
}

type imageBuild struct {
	provider *managedProvider
	client   proxmoxAPI
	image    store.ProjectImage
	access   gitAccess
	binary   []byte
	log      imageLog
}

// run ведёт сборку до ready или failed. Неуспешная сборка убирает свой
// контейнер; запись с журналом остаётся.
func (b *imageBuild) run(parent context.Context, createTask string) {
	ctx, cancel := context.WithTimeout(parent, imageBuildTimeout)
	defer cancel()
	st := b.provider.server.store
	flushed := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(flushed)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if text, changed := b.log.take(); changed {
					_ = st.SetProjectImageLog(context.WithoutCancel(ctx), b.image.ID, text)
				}
			}
		}
	}()
	err := b.build(ctx, createTask)
	close(stop)
	<-flushed
	durable, done := context.WithTimeout(context.WithoutCancel(parent), 2*time.Minute)
	defer done()
	if err != nil {
		reason := err.Error()
		if parent.Err() != nil {
			reason = "the server stopped during the build"
		} else if ctx.Err() != nil {
			reason = "the build exceeded its time limit"
		}
		b.log.printf("== build failed: %s\n", reason)
		_ = destroyImageContainer(durable, b.client, b.image)
		_ = st.SetProjectImageLog(durable, b.image.ID, b.log.text())
		_ = st.FinishProjectImage(durable, b.image.ID, "failed", reason)
		return
	}
	b.log.printf("== image ready\n")
	_ = st.SetProjectImageLog(durable, b.image.ID, b.log.text())
	_ = st.FinishProjectImage(durable, b.image.ID, "ready", "")
}

func (b *imageBuild) build(ctx context.Context, createTask string) error {
	owned := imageOwned(b.image)
	b.log.printf("== creating build container %d\n", b.image.CTID)
	if err := b.wait(ctx, createTask); err != nil {
		return fmt.Errorf("container creation failed: %s", managedProviderError(err))
	}
	upid, err := b.client.Start(ctx, owned)
	if err == nil {
		err = b.wait(ctx, upid)
	}
	if err != nil {
		return fmt.Errorf("container start failed: %s", managedProviderError(err))
	}
	session, err := b.connect(ctx)
	if err != nil {
		return err
	}
	defer session.Close()
	if err := session.WriteFile(ctx, agentBinaryPath, 0o755, b.binary); err != nil {
		return errors.New("agent binary transfer failed")
	}
	var script bytes.Buffer
	fmt.Fprintf(&script, "export REPO=%s REF=%s PLAYBOOK=%s\n", sshboot.Quote(b.access.repository), sshboot.Quote(b.image.Ref), sshboot.Quote(b.image.Playbook))
	if b.access.gateway {
		// Учётные данные идут в stdin сценария, не в командную строку, и
		// живут только в окружении этого процесса.
		fmt.Fprintf(&script, "export GIT_USER=%s GIT_PASS=%s\n", sshboot.Quote(b.access.credential.username), sshboot.Quote(b.access.credential.token))
	}
	script.WriteString(imageBuildScript)
	if err := session.Run(ctx, "bash -s", &script, &b.log); err != nil {
		var exit *sshboot.ExitError
		if errors.As(err, &exit) {
			return fmt.Errorf("the build script exited with status %d; see the log", exit.Status)
		}
		return errors.New("the build session was interrupted")
	}
	session.Close()
	b.log.printf("== stopping the container\n")
	upid, err = b.client.Shutdown(ctx, owned)
	if err == nil {
		err = b.wait(ctx, upid)
	}
	if err != nil {
		return fmt.Errorf("container shutdown failed: %s", managedProviderError(err))
	}
	if err := b.client.ConvertToTemplate(ctx, owned); err != nil {
		return fmt.Errorf("conversion to a template failed: %s", managedProviderError(err))
	}
	return nil
}

// wait ждёт задачу PVE дольше срока одного вызова клиента.
func (b *imageBuild) wait(ctx context.Context, upid string) error {
	for {
		step, cancel := context.WithTimeout(ctx, managedPassTimeout)
		err := b.client.WaitTask(step, upid)
		cancel()
		if err == nil || ctx.Err() != nil || !managedTaskPending(step, err) {
			return err
		}
	}
}

// connect ждёт адрес контейнера и sshd.
func (b *imageBuild) connect(ctx context.Context) (*sshboot.Session, error) {
	b.log.printf("== waiting for the container network\n")
	deadline := time.Now().Add(3 * time.Minute)
	for {
		addresses, err := b.client.Addresses(ctx, b.image.CTID)
		if err == nil && len(addresses) > 0 {
			step, cancel := context.WithTimeout(ctx, 40*time.Second)
			session, err := sshboot.Dial(step, b.provider.identity, sshboot.Target{Addr: netip.AddrPortFrom(addresses[0], 22)})
			cancel()
			if err == nil {
				return session, nil
			}
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return nil, errors.New("the build container did not become reachable over SSH")
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

// imageBuildScript играет плейбук проекта и запечатывает образ. Учётная
// запись actor и каталоги workspace создаются до плейбука, чтобы он мог
// класть файлы актору. machine-id очищается: у каждого клона он свой.
const imageBuildScript = `set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
echo "== installing ansible and git"
apt-get update -qq
apt-get install -y -qq --no-install-recommends ansible git ca-certificates >/dev/null
getent group actor >/dev/null || groupadd --gid 1000 actor
getent passwd actor >/dev/null || useradd --uid 1000 --gid 1000 --home-dir /workspace/home --no-create-home --shell /bin/bash actor
install -d -m 0755 /workspace
install -d -o 1000 -g 1000 -m 0700 /workspace/home /workspace/project
rm -rf /opt/brood-build
install -d -m 0700 /opt/brood-build
cd /opt/brood-build
echo "== fetching $REPO at $REF"
git init -q src
cd src
git remote add origin "$REPO"
if [ -n "${GIT_USER:-}" ]; then
  git -c credential.helper= -c 'credential.helper=!f() { echo "username=$GIT_USER"; echo "password=$GIT_PASS"; }; f' fetch -q --depth 1 origin "$REF"
else
  git fetch -q --depth 1 origin "$REF"
fi
git checkout -q FETCH_HEAD
unset GIT_USER GIT_PASS
if [ ! -f "$PLAYBOOK" ]; then
  echo "playbook $PLAYBOOK is not in the repository at $REF"
  exit 3
fi
echo "== running $PLAYBOOK"
ANSIBLE_NOCOLOR=1 ANSIBLE_FORCE_COLOR=0 PYTHONUNBUFFERED=1 ansible-playbook -i localhost, -c local "$PLAYBOOK"
echo "== sealing the image"
cd /
rm -rf /opt/brood-build
apt-get clean
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id
`

// imageLog держит хвост вывода сборки в пределах imageLogLimit.
type imageLog struct {
	mu      sync.Mutex
	buf     []byte
	changed bool
}

func (l *imageLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, bytes.ToValidUTF8(p, []byte("?"))...)
	if len(l.buf) > imageLogLimit {
		l.buf = append([]byte(nil), l.buf[len(l.buf)-imageLogLimit:]...)
		if i := bytes.IndexByte(l.buf, '\n'); i >= 0 {
			l.buf = l.buf[i+1:]
		}
	}
	l.changed = true
	return len(p), nil
}

func (l *imageLog) printf(format string, args ...any) { fmt.Fprintf(l, format, args...) }

func (l *imageLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.buf)
}

func (l *imageLog) take() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	changed := l.changed
	l.changed = false
	return string(l.buf), changed
}

// imageTemplate — шаблон нового воркера проекта в пуле: клон готового
// образа проекта, если он есть, иначе пусто (базовый шаблон пула).
func (m *managedProvider) imageTemplate(ctx context.Context, projectID string, pool store.ManagedPool) (store.ManagedTemplateSnapshot, error) {
	image, err := m.server.store.CurrentProjectImage(ctx, projectID, pool.UID)
	if errors.Is(err, store.ErrNotFound) {
		return store.ManagedTemplateSnapshot{}, nil
	}
	if err != nil {
		return store.ManagedTemplateSnapshot{}, err
	}
	return store.ManagedTemplateSnapshot{Profile: pool.Template.Profile, URI: store.ProjectImageURI(image.ID), Digest: pool.Template.Digest}, nil
}

// workerImage — образ, клоном которого создаётся воркер a; false — воркер
// из базового шаблона.
func (m *managedProvider) workerImage(ctx context.Context, a store.ManagedActor) (store.ProjectImage, bool, error) {
	id, ok := store.ProjectImageFromURI(a.Template.URI)
	if !ok {
		return store.ProjectImage{}, false, nil
	}
	image, err := m.server.store.ProjectImageByID(ctx, id)
	if err != nil {
		return image, true, fmt.Errorf("%w: the project image of this worker is gone", app.ErrConflict)
	}
	return image, true, nil
}
