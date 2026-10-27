// Троттлинг входа: счётчики неудач по username и по IP в памяти со
// скользящим окном. Персистентность не нужна: перезапуск сервера
// лишь сбрасывает ограничение.
package auth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LoginLimiter — ограничение частоты неудачных входов. Ключи —
// нормализованный username и IP клиента; счётчик любого ключа, дойдя
// до предела, блокирует вход до конца окна.
type LoginLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	fails   map[string][]time.Time
	lastGC  time.Time
	gcEvery time.Duration
}

// NewLoginLimiter — лимитер: max неудач за window (по умолчанию
// 10 за 10 минут).
func NewLoginLimiter(max int, window time.Duration) *LoginLimiter {
	if max <= 0 {
		max = 10
	}
	if window <= 0 {
		window = 10 * time.Minute
	}
	return &LoginLimiter{
		max:     max,
		window:  window,
		fails:   make(map[string][]time.Time),
		lastGC:  time.Now(),
		gcEvery: time.Minute,
	}
}

// keys — пара ключей счётчиков: username в нижнем регистре и IP.
func limiterKeys(username, ip string) []string {
	keys := make([]string, 0, 2)
	if username != "" {
		keys = append(keys, "user:"+strings.ToLower(username))
	}
	if ip != "" {
		keys = append(keys, "ip:"+ip)
	}
	return keys
}

// Allowed сообщает, можно ли пытаться войти с данным username и IP.
// Чистит хвосты счётчиков не чаще раза в gcEvery.
func (l *LoginLimiter) Allowed(username, ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked(now)
	for _, k := range limiterKeys(username, ip) {
		if len(l.windowSlice(k, now)) >= l.max {
			return false
		}
	}
	return true
}

// RecordFail учитывает неудачную попытку по обоим ключам.
func (l *LoginLimiter) RecordFail(username, ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked(now)
	for _, k := range limiterKeys(username, ip) {
		l.fails[k] = append(l.windowSlice(k, now), now)
	}
}

// Reset сбрасывает счётчики ключей (успешный вход).
func (l *LoginLimiter) Reset(username, ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range limiterKeys(username, ip) {
		delete(l.fails, k)
	}
}

// windowSlice возвращает неудачи ключа внутри окна; усекает хвост
// списка на месте.
func (l *LoginLimiter) windowSlice(key string, now time.Time) []time.Time {
	f := l.fails[key]
	cut := 0
	for cut < len(f) && now.Sub(f[cut]) > l.window {
		cut++
	}
	f = f[cut:]
	if len(f) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = f
	}
	return f
}

// gcLocked удаляет пустые и целиком устаревшие счётчики, чтобы карта
// не росла бесконечно от сканирования имён и IP.
func (l *LoginLimiter) gcLocked(now time.Time) {
	if now.Sub(l.lastGC) < l.gcEvery {
		return
	}
	l.lastGC = now
	for k := range l.fails {
		l.windowSlice(k, now)
	}
}

// ClientIP возвращает адрес TCP-пира без порта. Без явной настройки
// доверенного прокси заголовки пересылки не являются источником IP.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return strings.TrimSpace(r.RemoteAddr)
	}
	return host
}
