// Сборка сервера: хранилище, аутентификация, прикладной слой, адаптеры
// Substrate/Kubernetes, встроенная панель и маршруты. Server владеет
// закрываемыми зависимостями; Handler отдаёт готовый http.Handler.
package server

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/app"
	"brood.dev/go/brood/auth"
	"brood.dev/go/brood/proxy"
	"brood.dev/go/brood/store"
)

//go:embed static/index.html
var staticFS embed.FS

// Options — явные зависимости сборки; тесты подставляют фейк провижинера
// и logger. Нулевой Options годится: всё замещается умолчаниями.
type Options struct {
	// telegram — ритм поллеров Telegram и часы; нулевые поля берут умолчания.
	telegram telegramOptions
	// Logger — slog запросов и внутренних отказов; nil — умолчание slog.
	Logger *slog.Logger
	// argon2 — стоимость настоящего хэширования в тестах пакета;
	// нулевые параметры сохраняют auth.DefaultArgon2.
	argon2 auth.Argon2Params
}

// Server — собранный сервер: зависимости и маршруты без запуска сети.
type Server struct {
	store   *store.Store
	managed *managedProvider
	git     *gitGateway
	catalog *Catalog
	gateway *Gateway
	app     *app.Service
	auth    *auth.Service
	proxy   *proxy.Proxy
	views   actorViews
	// telegram — поллеры ботов, telegramChannel — канал шины между
	// владельцем роя и его агентами.
	telegram        *telegramBots
	telegramChannel *telegramChannel
	mux             *http.ServeMux
	cancel          context.CancelFunc

	// artifactsDir — каталог доставки артефактов бутстрапа гостя.
	artifactsDir string
	// actorGitDomain — домен Git-идентичности акторов (#249); пусто —
	// идентичность гостю не отдаётся.
	actorGitDomain string
	// actorModels — модели Claude Code акторов (#263); пусто — гостю не
	// отдаются, модель выбирает Claude Code.
	actorModels actormodel.Models

	managedLLM  http.Handler
	adminModels *adminModelClient
}

// New открывает хранилище по cfg.DBPath и собирает сервер целиком.
// Substrate и Kubernetes подключаются при настроенных переменных;
// не настроенный адаптер отказывает 503 на своих операциях. Close
// освобождает базу и каналы.
func New(ctx context.Context, cfg Config, opts Options) (*Server, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, err
	}
	srv, err := newFromStore(ctx, cfg, opts, st)
	if err != nil {
		st.Close()
		return nil, err
	}
	return srv, nil
}

// NewFromStore собирает сервер поверх уже открытого хранилища: тестам
// хватает httptest без второй базы.
func NewFromStore(cfg Config, opts Options, st *store.Store) (*Server, error) {
	return newFromStore(context.Background(), cfg, opts, st)
}

func newFromStore(ctx context.Context, cfg Config, opts Options, st *store.Store) (*Server, error) {
	if err := cfg.validateAdapters(); err != nil {
		return nil, err
	}
	catalog, err := OpenCatalog(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	if err := st.FailBuildingProjectImages(ctx); err != nil {
		catalog.Close()
		return nil, errors.New("project image recovery failed")
	}
	if err := st.RecoverActorOperations(ctx); err != nil {
		catalog.Close()
		return nil, errors.New("workspace operation recovery failed")
	}
	srv := &Server{store: st, catalog: catalog, views: actorViews{tickets: make(map[string]actorView)}, git: newGitGateway(cfg.gitRemotes), artifactsDir: cfg.artifactsDir, actorGitDomain: cfg.ActorGitDomain, actorModels: cfg.ActorModels}
	srv.telegram = newTelegramBots(st, cfg, opts.telegram)
	srv.telegramChannel = newTelegramChannel(srv, srv.telegram)
	srv.telegram.channel = srv.telegramChannel
	srv.managed = &managedProvider{server: srv, registry: cfg.proxmox}
	if err := srv.managed.initialize(cfg); err != nil {
		catalog.Close()
		return nil, err
	}

	srv.gateway = StoreGateway(st, catalog)
	srv.app = app.NewService(srv.gateway, time.Now, store.NewID)

	authCfg := auth.Config{Secure: cfg.SecureCookies, Secret: cfg.SessionSecret, Argon2: opts.argon2}
	srv.auth = auth.NewService(st, authCfg, time.Now)

	// Actor ingress shares session/CSRF protection. A lifecycle-bound
	// ticker rechecks authorization and the original session, including
	// ordinary requests still waiting for upstream headers or bodies.
	srv.proxy = proxy.New(proxy.Config{
		Swarms:    srv.gateway,
		Authorize: srv.app.Can,
		// Две native-проверки по 5 секунд должны уложиться в минутный отзыв.
		RecheckInterval: 45 * time.Second,
		CurrentUser:     srv.actorViewUser,
		RequestHook:     srv.actorViewRequest,
		ResponseHook:    srv.actorViewResponse,
		BeginUpgrade:    srv.beginManagedTerminal,
		Upstream:        &managedResolver{server: srv},
		DialContext:     srv.managed.dial,
		UpstreamValid:   srv.managed.validTarget,
		Revalidate:      srv.validProxySession,
	})
	srv.mux = http.NewServeMux()
	srv.mount()
	srv.mountProxmoxPools(cfg.proxmox)
	srv.mountProxmoxConnections()
	srv.mountProjectSecrets()
	srv.mountGitCredentials()
	srv.mountGitRepositories()
	srv.mountProjectOwnership()
	srv.mountProjectImages()
	srv.mountLists()
	srv.mountTelegramProfile()
	srv.mountLLM(cfg)
	srv.adminModels = newAdminModelClient(cfg)
	srv.managedLLM = srv.managedLLMHandler(cfg)
	proxyCtx, cancel := context.WithCancel(ctx)
	srv.cancel = cancel
	srv.proxy.Start(proxyCtx)
	srv.managed.start(proxyCtx)
	srv.telegram.start(proxyCtx)
	return srv, nil
}

func (s *Server) validProxySession(r *http.Request) bool {
	if v, ok := r.Context().Value(actorViewKey{}).(actorView); ok {
		return s.validActorView(v)
	}
	cookie, err := r.Cookie(auth.CookieSession)
	if err != nil {
		return false
	}
	user, ok := auth.UserFromContext(r.Context())
	if !ok {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, _, err := auth.NewSessions(s.store, 0).Load(ctx, cookie.Value, time.Now())
	return err == nil && id == user.ID
}

// Close освобождает все зависимости сервера.
func (s *Server) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	// Диспетчер шагов пишет исход в базу и зовёт hub: закрывается раньше них.
	// Поллеры и отправитель Telegram пишут в базу: остановлены до её
	// закрытия.
	s.telegram.close()
	s.proxy.Close()
	s.managed.close()
	var errs []error
	errs = append(errs, s.catalog.Close(), s.store.Close())
	return errors.Join(errs...)
}

// Handler возвращает собранный обработчик: middleware сессий и CSRF
// поверх всех маршрутов, /healthz и статика — без аутентификации.
func (s *Server) Handler() http.Handler {
	browser := s.actorViewHandler(s.browserProtection())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicProjectSecretPath(r.URL.Path) {
			s.serveGuestProjectSecrets(w, r)
			return
		}
		if publicWorkspacePath(r.URL.Path) {
			s.serveGuestWorkspace(w, r)
			return
		}
		if publicArtifactsPath(r.URL.Path) {
			s.serveGuestArtifacts(w, r)
			return
		}
		if publicManagedLLMPath(r.URL.Path) {
			s.managedLLM.ServeHTTP(w, r)
			return
		}
		if publicManagedGitPath(r.URL.Path) {
			s.serveManagedGit(w, r)
			return
		}
		if publicGuestPath(r.URL.Path) {
			s.guestRoute(w, r)
			return
		}
		if publicLLMPath(r.URL.Path) {
			// Native clients authenticate with their own backend bearer key,
			// not browser sessions or CSRF. Never parse their session cookies.
			s.mux.ServeHTTP(w, r)
			return
		}
		browser.ServeHTTP(w, r)
	})
}

// mount регистрирует маршруты сервера.
func (s *Server) mount() {
	s.mux.HandleFunc("POST /api/commands/{tool}", s.handleCommand)
	// Проба готовности и панель — до аутентификации (mount в mux до
	// middleware, но healthz обрабатывается здесь же, анонимно).
	s.mux.HandleFunc("GET /healthz", s.handleHealthz)
	s.auth.Mount(s.mux)
	s.mux.HandleFunc("GET /api/admin/users", s.handleAdminUsers)
	s.mux.HandleFunc("GET /api/projects", s.handleProjects)
	s.mux.HandleFunc("POST /api/projects", s.handleProjects)
	s.mux.HandleFunc("GET /api/projects/{id}", s.handleProjectDetail)
	s.mux.HandleFunc("PUT /api/projects/{id}/capacity", s.handleShardCapacity)
	s.mux.HandleFunc("DELETE /api/projects/{id}", s.handleProjectDelete)
	s.mux.HandleFunc("GET /api/projects/{id}/members", s.handleMemberList)
	s.mux.HandleFunc("POST /api/projects/{id}/members", s.handleMembers)
	s.mux.HandleFunc("DELETE /api/projects/{id}/members/{user}", s.handleMemberDelete)
	s.mux.HandleFunc("GET /api/projects/{id}/swarms", s.handleProjectSwarms)
	s.mux.HandleFunc("POST /api/projects/{id}/swarms", s.handleProjectSwarms)
	s.mux.HandleFunc("GET /api/swarms/{id}", s.handleSwarm)
	s.mux.HandleFunc("DELETE /api/swarms/{id}", s.handleSwarm)
	s.mux.HandleFunc("POST /api/swarms/{id}/transfer", s.handleSwarmTransfer)
	// Actor/template/policy роя, воркеры и глобальная политика — тонкий
	// прокси к ateapi с проверкой владения роем (#32); он же подключает
	// маршруты proxy под /api/swarms/{id}/proxy/.
	s.mountActorAPI()
	s.mountStatic()
}

// handleHealthz — проба готовности без аутентификации: сервер отвечает
// и достаёт до своей базы; отказ базы — 503 с причиной.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Дешёвый aggregate-запрос: ошибиться может только база.
	if _, err := s.store.CountActiveAdmins(ctx); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("database unavailable: " + err.Error() + "\n"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// mountStatic раздаёт встроенную панель: единый самодостаточный index.html
// с fetch-логином по CONTRACT.md.
func (s *Server) mountStatic() {
	index, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		panic("server: embedded index.html: " + err.Error())
	}
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Корень — панель; всё неперехваченное — 404.
		if r.URL.Path != "/" || r.Method != http.MethodGet {
			writeDetail(w, http.StatusNotFound, "not found")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(index)
	})
}
