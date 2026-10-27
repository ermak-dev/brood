// HTTP-слой аутентификации: middleware сессий и CSRF, эндпоинты входа
// и управления пользователями по контракту CONTRACT.md. Ошибки — JSON
// {"detail": ...} без трассировок и секретов; пароли и токены в логи
// не попадают.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"brood.dev/go/brood/store"
)

// Config — настройки аутентификации. Secure управляет атрибутом Secure
// обеих cookie: false допустимо только явной настройкой локальной
// разработки без TLS. Secret подписывает CSRF-токен анонимных
// bootstrap-запросов; пустой секрет заменяется случайным на старте
// процесса. Анонимные токены не переживают перезапуск с новым секретом.
type Config struct {
	Secure bool
	Secret []byte
	// Argon2 — параметры хэширования; нулевые означают DefaultArgon2.
	Argon2 Argon2Params
}

// Service — HTTP-аутентификация поверх хранилища.
type Service struct {
	store     *store.Store
	sessions  *Sessions
	limiter   *LoginLimiter
	cfg       Config
	newUserID func() string
	now       func() time.Time
}

// NewService собирает сервис аутентификации.
func NewService(st *store.Store, cfg Config, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	secret := cfg.Secret
	if len(secret) == 0 {
		// Случайный секрет процесса: анонимный CSRF-токен не обязан
		// переживать перезапуск, серверной сессии у анонима нет.
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			panic("auth: read secret: " + err.Error())
		}
		cfg.Secret = secret
	}
	return &Service{
		store:     st,
		sessions:  NewSessions(st, 0),
		limiter:   NewLoginLimiter(0, 0),
		cfg:       cfg,
		newUserID: store.NewID,
		now:       now,
	}
}

// hashPassword хэширует пароль с параметрами конфигурации.
func (s *Service) hashPassword(password string) (string, error) {
	return HashPassword(password, s.cfg.Argon2)
}

// Ключи контекста запроса: пользователь и логгер.
type ctxKey int

// Ключи контекста запроса: пользователь, логгер и CSRF-секрет сессии.
const (
	ctxUser ctxKey = iota
	ctxLogger
	ctxCSRF
)

// UserFromContext возвращает пользователя аутентифицированного
// запроса. Права не замораживаются в сессии: middleware читает запись
// пользователя из базы на каждом запросе.
func UserFromContext(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(ctxUser).(store.User)
	return u, ok
}

// WithLogger кладёт логгер в контекст: middleware дополнит его user id.
func WithLogger(ctx context.Context, lg *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxLogger, lg)
}

// LoggerFromContext достаёт логгер запроса (nil, если не клали).
func LoggerFromContext(ctx context.Context) *slog.Logger {
	lg, _ := ctx.Value(ctxLogger).(*slog.Logger)
	return lg
}

// Mount регистрирует маршруты аутентификации и управления
// пользователями в mux.
func (s *Service) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/auth/session", s.handleSessionView)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("POST /api/auth/password", s.handleChangePassword)
	mux.HandleFunc("POST /api/admin/users", s.handleAdminCreateUser)
	mux.HandleFunc("POST /api/admin/users/{username}/disable", s.handleAdminSetDisabled(true))
	mux.HandleFunc("POST /api/admin/users/{username}/enable", s.handleAdminSetDisabled(false))
	mux.HandleFunc("POST /api/admin/users/{username}/password-reset", s.handleAdminPasswordReset)
	mux.HandleFunc("POST /api/admin/users/{username}/role", s.handleAdminRole)
}

// Middleware — LoadAndSave сессий и CSRF: читает session cookie,
// загружает сессию по хэшу, отказывает истёкшим, проверяет CSRF на
// изменяющих методах. Пользователь кладётся в контекст, логгер
// запроса дополняется его идентификатором.
func (s *Service) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		cookie, err := r.Cookie(CookieSession)
		switch {
		case errors.Is(err, http.ErrNoCookie):
			// Аноним: серверной сессии нет.
		case err != nil:
			writeError(w, http.StatusBadRequest, "malformed session cookie")
			return
		case cookie.Value != "":
			userID, csrf, err := s.sessions.Load(ctx, cookie.Value, s.now())
			if errors.Is(err, ErrSessionNotFound) {
				// Истёкшая или отозванная сессия равна анониму; cookie
				// чистим, чтобы браузер перестал её слать.
				s.clearSessionCookie(w)
			} else if err != nil {
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			} else {
				// Сессия действительна: права читаем из записи пользователя
				// на каждом запросе, а не из сессии.
				user, err := s.store.GetUser(ctx, userID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					writeError(w, http.StatusInternalServerError, "internal error")
					return
				}
				if err == nil && !user.Disabled {
					ctx = context.WithValue(ctx, ctxUser, user)
					ctx = context.WithValue(ctx, ctxCSRF, csrf)
					if lg := LoggerFromContext(ctx); lg != nil {
						// Идентификатор пользователя в каждой записи лога
						// запроса; пароли и токены не логируются нигде.
						ctx = context.WithValue(ctx, ctxLogger,
							lg.With("auth_user_id", user.ID).
								With("auth_username", user.Username))
					}
				}
				// Заблокированный или удалённый пользователь: cookie ещё
				// жив, но запрос остаётся анонимным.
			}
		}

		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.csrfOK(w, r, ctx) {
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// csrfOK защищает в том числе анонимный login от подмены учётной записи.
// Origin обязателен для login; при наличии проверяется и для остальных
// изменений. Заголовкам прокси не доверяем: внешний HTTPS задаёт Secure.
func (s *Service) csrfOK(w http.ResponseWriter, r *http.Request, ctx context.Context) bool {
	login := r.URL.Path == "/api/auth/login"
	if (login || r.Header.Get("Origin") != "") && !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "same-origin request required")
		return false
	}
	if sessCSRF, ok := ctx.Value(ctxCSRF).(string); ok {
		if sessCSRF != "" && subtleEqual(sessCSRF, r.Header.Get(HeaderCSRF)) {
			return true
		}
		writeError(w, http.StatusForbidden, "CSRF token missing or incorrect")
		return false
	}
	if login {
		cookie, err := r.Cookie(CookieCSRF)
		if err != nil || !s.validAnonymousCSRF(cookie.Value) || !subtleEqual(cookie.Value, r.Header.Get(HeaderCSRF)) {
			writeError(w, http.StatusForbidden, "CSRF token missing or incorrect")
			return false
		}
	}
	return true
}

func (s *Service) sameOrigin(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Origin"))
	scheme := "http"
	if s.cfg.Secure || r.TLS != nil {
		scheme = "https"
	}
	return err == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) &&
		u.Host != "" && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func (s *Service) signAnonymousCSRF(nonce string) string {
	mac := hmac.New(sha256.New, s.cfg.Secret)
	mac.Write([]byte("anonymous csrf:" + nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Service) validAnonymousCSRF(token string) bool {
	nonce, signature, ok := strings.Cut(token, ".")
	return ok && len(nonce) == 43 && len(signature) == 64 && subtleEqual(signature, s.signAnonymousCSRF(nonce))
}

// subtleEqual — сравнение за постоянное время.
func subtleEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

// handleSessionView — GET /api/auth/session: анониму выдаёт csrf cookie
// и authenticated=false, вошедшему — данные пользователя из записи в
// базе (текущие, не из сессии).
func (s *Service) handleSessionView(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, ok := UserFromContext(r.Context())
	if !ok {
		nonce, err := newCSRFToken()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		s.setCSRFCookie(w, nonce+"."+s.signAnonymousCSRF(nonce))
		writeJSON(w, http.StatusOK, sessionResponse{Authenticated: false})
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{
		Authenticated: true,
		User: &userInfo{
			Username:    user.Username,
			DisplayName: strOrEmpty(user.DisplayName),
			IsAdmin:     user.IsAdmin,
		},
	})
}

// userInfo — публичные данные пользователя.
type userInfo struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	IsAdmin     bool   `json:"is_admin"`
}

// sessionResponse — ответ GET /api/auth/session.
type sessionResponse struct {
	Authenticated bool      `json:"authenticated"`
	User          *userInfo `json:"user,omitempty"`
}

// handleLogin — POST /api/auth/login {"username","password"}.
// Успех: 204, новая session cookie и ротация csrf. Отказ: 401 без
// раскрытия, существует ли username; лимит — 429.
func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Username == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	ip := ClientIP(r)
	if !s.limiter.Allowed(body.Username, ip, s.now()) {
		writeError(w, http.StatusTooManyRequests,
			"too many failed login attempts, try again later")
		return
	}

	ok, u := s.checkCredentials(r.Context(), body.Username, body.Password)
	if !ok || u.Disabled {
		// Любой отказ входа — неудача для лимитера, ответ одинаковый.
		s.limiter.RecordFail(body.Username, ip, s.now())
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	s.limiter.Reset(body.Username, ip)

	// Ротация: старая сессия cookie (если была) удаляется, заводится
	// новая с новым токеном и csrf-секретом.
	oldCookie, _ := r.Cookie(CookieSession)
	token, csrf, err := s.rotate(r.Context(), u, cookieToken(oldCookie))
	if errors.Is(err, errCredentialsChanged) {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.setSessionCookie(w, token)
	s.setCSRFCookie(w, csrf)
	w.WriteHeader(http.StatusNoContent)
}

// checkCredentials ищет пользователя и сверяет пароль. Несуществующий
// username проходит ту же Argon2id-проверку по хэшу-приманке: тайминг
// не выдаёт существование имени.
func (s *Service) checkCredentials(ctx context.Context, username, password string) (bool, store.User) {
	u, err := s.store.GetUserByUsername(ctx, username)
	if errors.Is(err, store.ErrNotFound) {
		dummyVerify(password)
		return false, store.User{}
	}
	if err != nil {
		return false, store.User{}
	}
	valid, err := VerifyPassword(password, u.PasswordHash)
	if err != nil || !valid {
		// Битый хэш в базе — тот же отказ, что неверный пароль:
		// внешний ответ одинаков.
		return false, store.User{}
	}
	return true, u
}

var errCredentialsChanged = errors.New("credentials changed")

// Проверка под блокировкой записи не позволяет устаревшей проверке
// пароля отменить результат конкурентного сброса или блокировки.
func (s *Service) checkCurrentCredentials(ctx context.Context, checked store.User) error {
	current, err := s.store.GetUser(ctx, checked.ID)
	if errors.Is(err, store.ErrNotFound) {
		return errCredentialsChanged
	}
	if err != nil {
		return err
	}
	if current.Disabled || !subtleEqual(current.PasswordHash, checked.PasswordHash) {
		return errCredentialsChanged
	}
	return nil
}

// rotate атомарно перепроверяет учётные данные и заменяет сессию.
func (s *Service) rotate(ctx context.Context, checked store.User, oldToken string) (token, csrf string, err error) {
	err = s.store.WithTx(ctx, func(ctx context.Context) error {
		if err := s.checkCurrentCredentials(ctx, checked); err != nil {
			return err
		}
		if oldToken != "" {
			if err := s.sessions.Revoke(ctx, oldToken); err != nil {
				return err
			}
		}
		var err error
		token, csrf, err = s.sessions.Start(ctx, checked.ID, s.now())
		return err
	})
	if err != nil {
		return "", "", fmt.Errorf("rotate session: %w", err)
	}
	return token, csrf, nil
}

// handleLogout — POST /api/auth/logout: удаляет серверную сессию и
// чистит cookie. Анонимному запросу тоже 204: logout идемпотентен.
func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(CookieSession); err == nil && cookie.Value != "" {
		if err := s.sessions.Revoke(r.Context(), cookie.Value); err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	s.clearSessionCookie(w)
	s.clearCSRFCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// handleChangePassword — POST /api/auth/password: смена собственного
// пароля, требует текущий пароль. Все сессии пользователя отзываются,
// текущему браузеру выдаётся новая.
func (s *Service) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.CurrentPassword == "" || body.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "current_password and new_password are required")
		return
	}
	valid, err := VerifyPassword(body.CurrentPassword, user.PasswordHash)
	if err != nil || !valid {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	hash, err := s.hashPassword(body.NewPassword)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid new password")
		return
	}
	token, csrf, err := s.changePassword(r.Context(), user, hash)
	if errors.Is(err, errCredentialsChanged) {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.setSessionCookie(w, token)
	s.setCSRFCookie(w, csrf)
	w.WriteHeader(http.StatusNoContent)
}

// changePassword атомарно меняет хэш, отзывает все сессии и заводит
// одну новую взамен текущей.
func (s *Service) changePassword(ctx context.Context, checked store.User, hash string) (token, csrf string, err error) {
	err = s.store.WithTx(ctx, func(ctx context.Context) error {
		if err := s.checkCurrentCredentials(ctx, checked); err != nil {
			return err
		}
		if err := s.store.SetPasswordHash(ctx, checked.ID, hash); err != nil {
			return err
		}
		if _, err := s.sessions.RevokeAllOfUser(ctx, checked.ID); err != nil {
			return err
		}
		var err error
		token, csrf, err = s.sessions.Start(ctx, checked.ID, s.now())
		return err
	})
	if err != nil {
		return "", "", fmt.Errorf("change password: %w", err)
	}
	return token, csrf, nil
}

// requireAdmin отказывает анониму (401) и не-администратору (403).
// Права читаются из записи пользователя на каждом запросе.
func (s *Service) requireAdmin(w http.ResponseWriter, r *http.Request) (store.User, bool) {
	user, ok := UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return store.User{}, false
	}
	if !user.IsAdmin {
		writeError(w, http.StatusForbidden, "administrator access required")
		return store.User{}, false
	}
	return user, true
}

// handleAdminCreateUser — POST /api/admin/users: администратор заводит
// пользователя.
func (s *Service) handleAdminCreateUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var body struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		DisplayName string `json:"display_name"`
		IsAdmin     bool   `json:"is_admin"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Username == "" || body.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	hash, err := s.hashPassword(body.Password)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid password")
		return
	}
	display := body.DisplayName
	u := store.User{
		ID:           s.newUserID(),
		Username:     body.Username,
		DisplayName:  &display,
		PasswordHash: hash,
		IsAdmin:      body.IsAdmin,
	}
	if err := s.store.CreateUser(r.Context(), u); err != nil {
		if store.IsUniqueViolation(err) {
			writeError(w, http.StatusConflict, "user already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, userInfo{
		Username:    u.Username,
		DisplayName: body.DisplayName,
		IsAdmin:     u.IsAdmin,
	})
}

// handleAdminSetDisabled — блокировка и разблокировка пользователя.
// Блокировка отзывает его сессии и не даёт снять последнего активного
// администратора.
func (s *Service) handleAdminSetDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.requireAdmin(w, r); !ok {
			return
		}
		username := r.PathValue("username")
		target, err := s.store.GetUserByUsername(r.Context(), username)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		err = s.store.WithTx(r.Context(), func(ctx context.Context) error {
			if err := s.store.SetUserDisabled(ctx, target.ID, disabled); err != nil {
				return err
			}
			if disabled {
				// Блокировка немедленно отзывает все сессии.
				_, err := s.sessions.RevokeAllOfUser(ctx, target.ID)
				return err
			}
			return nil
		})
		if errors.Is(err, store.ErrLastActiveAdmin) {
			writeError(w, http.StatusConflict, "cannot disable the last active admin")
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleAdminPasswordReset — POST /api/admin/users/{username}/password-reset:
// администратор задаёт новый пароль, все сессии пользователя отзываются.
func (s *Service) handleAdminPasswordReset(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var body struct {
		NewPassword string `json:"new_password"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.NewPassword == "" {
		writeError(w, http.StatusBadRequest, "new_password is required")
		return
	}
	username := r.PathValue("username")
	target, err := s.store.GetUserByUsername(r.Context(), username)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	hash, err := s.hashPassword(body.NewPassword)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid new password")
		return
	}
	err = s.store.WithTx(r.Context(), func(ctx context.Context) error {
		if err := s.store.SetPasswordHash(ctx, target.ID, hash); err != nil {
			return err
		}
		// Сброс пароля отзывает все сессии пользователя.
		_, err := s.sessions.RevokeAllOfUser(ctx, target.ID)
		return err
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// cookieToken возвращает значение cookie или пустую строку.
func cookieToken(c *http.Cookie) string {
	if c == nil {
		return ""
	}
	return c.Value
}

// strOrEmpty разыменовывает необязательную строку.
func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// decodeJSON читает ограниченное JSON-тело; при ошибке сам пишет ответ.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "malformed JSON body")
		return false
	}
	return true
}

// writeJSON пишет успешный ответ JSON.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError пишет ошибку контракта: {"detail": "<английское сообщение>"}.
func writeError(w http.ResponseWriter, status int, detail string) {
	writeJSON(w, status, struct {
		Detail string `json:"detail"`
	}{Detail: detail})
}

// sessionCookieMaxAge — срок cookie сессии в секундах.
func (s *Service) sessionCookieMaxAge() int {
	return int(s.sessions.ttl.Seconds())
}

// setSessionCookie выдаёт HttpOnly cookie сессии.
func (s *Service) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieSession,
		Value:    token,
		Path:     "/",
		MaxAge:   s.sessionCookieMaxAge(),
		HttpOnly: true,
		Secure:   s.cfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// setCSRFCookie выдаёт cookie csrf без HttpOnly: панель читает значение.
func (s *Service) setCSRFCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieCSRF,
		Value:    token,
		Path:     "/",
		MaxAge:   s.sessionCookieMaxAge(),
		HttpOnly: false,
		Secure:   s.cfg.Secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie удаляет cookie сессии у браузера.
func (s *Service) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieSession, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.Secure, SameSite: http.SameSiteLaxMode,
	})
}

// clearCSRFCookie удаляет cookie csrf у браузера.
func (s *Service) clearCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: CookieCSRF, Value: "", Path: "/", MaxAge: -1,
		Secure: s.cfg.Secure, SameSite: http.SameSiteLaxMode,
	})
}
