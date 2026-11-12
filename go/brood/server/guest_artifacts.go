// Доставка артефактов бутстрапа гостя (#155): манифест и файлы из
// BROOD_ARTIFACTS_DIR. Образ песочницы несёт только bootstrap-бинарь;
// root supervisor скачивает полный релиз у своего сервера.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	guestArtifactsPrefix       = "/api/proxmox/artifacts/"
	guestArtifactsSetting      = "BROOD_ARTIFACTS_DIR"
	guestArtifactsLimit        = 512 << 20
	guestArtifactsManifestFile = "manifest.json"
	guestArtifactsManifestMax  = 16 << 10
	guestArtifactsDetail       = "guest artifact delivery unavailable"
	// guestArtifactsStream — общий срок запроса потока файла: артефакт до
	// 512MiB по медленному каналу гостя, дольше — обрывает клиент.
	guestArtifactsStream = 10 * time.Minute
)

// guestArtifactNames — закрытый список имён артефактов бутстрапа.
var guestArtifactNames = []string{"bro", "brood-guest", "claude"}

// guestArtifactDigestPattern — sha256: 64 строчных hex-символа.
var guestArtifactDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// guestArtifactEntry — строка манифеста: имя, размер и digest файла.
type guestArtifactEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// guestArtifactManifest — манифест каталога артефактов.
type guestArtifactManifest struct {
	Artifacts []guestArtifactEntry `json:"artifacts"`
}

// publicArtifactsPath — машинные маршруты артефактов вне cookie/CSRF
// middleware.
func publicArtifactsPath(path string) bool {
	return strings.HasPrefix(path, guestArtifactsPrefix)
}

// guestArtifactsRest принимает хвост пути: пустой (манифест) или одно имя
// из закрытого списка (файл); всё остальное — 404 до авторизации.
func guestArtifactsRest(rest []string) bool {
	if len(rest) == 0 {
		return true
	}
	if len(rest) != 1 {
		return false
	}
	for _, name := range guestArtifactNames {
		if rest[0] == name {
			return true
		}
	}
	return false
}

// readGuestArtifacts перечитывает манифест каталога dir на каждый запрос
// (обновление артефактов не требует рестарта сервера) и проверяет его:
// ровно закрытый список имён без повторов, размер в (0, 512MiB], digest
// из строчных hex64; файл каждого имени существует, regular и совпадает
// размером. sha256 не пересчитывается: digest манифесту доверяет, его
// считает CI при сборке. Любое несоответствие — один отказ без деталей.
// Возвращает записи, отсортированные по имени: ответ детерминированный.
func readGuestArtifacts(dir string) (guestArtifactManifest, bool) {
	var manifest guestArtifactManifest
	data, err := os.ReadFile(filepath.Join(dir, guestArtifactsManifestFile))
	if err != nil || len(data) > guestArtifactsManifestMax {
		return manifest, false
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, false
	}
	if len(manifest.Artifacts) != len(guestArtifactNames) {
		return manifest, false
	}
	entries := append([]guestArtifactEntry(nil), manifest.Artifacts...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	for i, entry := range entries {
		if entry.Name != guestArtifactNames[i] || entry.Size <= 0 || entry.Size > guestArtifactsLimit || !guestArtifactDigestPattern.MatchString(entry.SHA256) {
			return manifest, false
		}
		info, err := os.Stat(filepath.Join(dir, entry.Name))
		if err != nil || !info.Mode().IsRegular() || info.Size() != entry.Size {
			return manifest, false
		}
	}
	manifest.Artifacts = entries
	return manifest, true
}

// guestReleaseIDDomain открывает канонический текст id набора (#184), как
// у гостя.
const guestReleaseIDDomain = "brood guest release v1\n"

// guestReleaseID — id набора гостевых программ проверенного манифеста (#184):
// sha256 строки "brood guest release v1" и строк "имя размер sha256" в
// порядке закрытого списка. Этим id bootstrap называет releases/<id> в CT
// (guest.ArtifactManifest.ReleaseID); серверу пакет гостя не нужен, а
// совпадение формул держит TestGuestReleaseIDMatchesBootstrap.
func guestReleaseID(manifest guestArtifactManifest) string {
	digest := sha256.New()
	io.WriteString(digest, guestReleaseIDDomain)
	for _, a := range manifest.Artifacts {
		io.WriteString(digest, a.Name+" "+strconv.FormatInt(a.Size, 10)+" "+a.SHA256+"\n")
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// currentGuestRelease — id набора, который сервер раздаёт гостям сейчас
// (#133), или false без настройки и проверенного манифеста: тогда bootstrap
// #184 при старте CT отказывает fail closed.
func (s *Server) currentGuestRelease() (string, bool) {
	if s.artifactsDir == "" {
		return "", false
	}
	manifest, ok := readGuestArtifacts(s.artifactsDir)
	if !ok {
		return "", false
	}
	return guestReleaseID(manifest), true
}

// serveGuestArtifacts отдаёт root supervisor гостя манифест и файлы
// артефактов бутстрапа: GET /api/proxmox/artifacts/{uid}/{generation}[/name].
// Отсутствие настройки, каталога, манифеста или несовпадение файла — 503
// одной статичной причиной, без деталей файловой системы; неизвестное имя
// и кривой путь — 404 до авторизации.
func (s *Server) serveGuestArtifacts(w http.ResponseWriter, r *http.Request) {
	// Общий срок покрывает и поток файла: медленный гость качает до 10 минут,
	// быстрее — клиентский timeout.
	ctx, cancel := context.WithTimeout(r.Context(), guestArtifactsStream)
	defer cancel()
	g, rest, ok := s.openGuestRuntime(ctx, w, r, guestArtifactsPrefix, guestArtifactsDetail, guestArtifactsRest)
	if !ok {
		return
	}
	if s.artifactsDir == "" {
		writeDetail(w, 503, guestArtifactsDetail)
		return
	}
	manifest, ok := readGuestArtifacts(s.artifactsDir)
	if !ok {
		writeDetail(w, 503, guestArtifactsDetail)
		return
	}
	if len(rest) == 0 {
		s.writeGuestArtifactManifest(w, ctx, g, manifest)
		return
	}
	s.serveGuestArtifactFile(w, r, ctx, g, rest[0])
}

// writeGuestArtifactManifest отдаёт проверенный манифест детерминированным
// JSON; заголовки маршрута уже выставлены openGuestRuntime.
func (s *Server) writeGuestArtifactManifest(w http.ResponseWriter, ctx context.Context, g guestRuntime, manifest guestArtifactManifest) {
	data, err := json.Marshal(manifest)
	if err != nil {
		writeDetail(w, 503, guestArtifactsDetail)
		return
	}
	if !s.guestRuntimeCurrent(ctx, g) {
		writeDetail(w, 401, "guest authorization denied")
		return
	}
	writeGuestRuntime(w, data)
}

// serveGuestArtifactFile отдаёт один артефакт потоком: application/octet-stream
// и Content-Length через ServeContent. Привязку гостя проверяем один раз,
// после открытия файла и до первого байта: повторная проверка посреди
// десятиминутного потока не входит в эту задачу.
func (s *Server) serveGuestArtifactFile(w http.ResponseWriter, r *http.Request, ctx context.Context, g guestRuntime, name string) {
	file, err := os.Open(filepath.Join(s.artifactsDir, name))
	if err != nil {
		writeDetail(w, 503, guestArtifactsDetail)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > guestArtifactsLimit {
		writeDetail(w, 503, guestArtifactsDetail)
		return
	}
	if !s.guestRuntimeCurrent(ctx, g) {
		writeDetail(w, 401, "guest authorization denied")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, name, time.Time{}, file)
}

// loadGuestArtifactsDir читает настройку каталога артефактов: свойство
// машины, без дефолта. Пусто — доставка не настроена; непустое значение —
// абсолютный путь с непустым basename (чтение откажется 503, если каталог
// недоступен; symlink-проверки на этапе настройки нет).
func loadGuestArtifactsDir(get getenv) (string, error) {
	dir := envGet(get, guestArtifactsSetting)
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) || strings.HasSuffix(dir, string(filepath.Separator)) {
		return "", &ConfigError{Setting: guestArtifactsSetting,
			Detail: "must be an absolute directory path with a nonempty basename"}
	}
	return dir, nil
}
