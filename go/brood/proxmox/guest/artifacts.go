//go:build linux

package guest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

// artifactNames — закрытый список артефактов Bootstrap (#155): точные имена
// файлов набора /opt/brood/releases/<id> (на него указывает /opt/brood/bin,
// #184), каждое ровно один раз; порядок алфавитный, он же порядок ReleaseID.
var artifactNames = [3]string{"bro", "brood-guest", "claude"}

const artifactManifestLimit = 16 << 10
const artifactMaxSize = 512 << 20
const artifactFetchTimeout = 10 * time.Minute

// Artifact описывает один двоичный артефакт сервера.
type Artifact struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// ArtifactManifest — манифест Bootstrap: ровно один объект с одним полем
// artifacts и всеми тремя именами закрытого списка.
type ArtifactManifest struct {
	Artifacts []Artifact `json:"artifacts"`
}

func artifactFailure() error { return errors.New("guest artifact delivery unavailable or unsafe") }

// ParseArtifactManifest разбирает манифест строго: точный объект, единственный
// массив artifacts, записи только name/size/sha256, без неизвестных и дублей
// ключей и имён, все три имени закрытого списка ровно один раз, ограниченный
// размер и строчный hex256 digest, без документов после основного.
func ParseArtifactManifest(data []byte) (ArtifactManifest, error) {
	fail := func() (ArtifactManifest, error) { return ArtifactManifest{}, artifactFailure() }
	// Манифест — точный JSON-объект без обводки пробелами.
	if len(data) == 0 || data[0] != '{' || data[len(data)-1] != '}' {
		return fail()
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return fail()
	}
	seenField, seenArtifacts := false, false
	entries := []json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || key != "artifacts" {
			return fail()
		}
		if seenField {
			return fail()
		}
		seenField = true
		if decoder.Decode(&entries) != nil {
			return fail()
		}
		seenArtifacts = true
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || !seenArtifacts {
		return fail()
	}
	if len(entries) != len(artifactNames) {
		return fail()
	}
	seenNames := map[string]bool{}
	manifest := ArtifactManifest{}
	for _, raw := range entries {
		artifact, err := parseArtifactEntry(raw)
		if err != nil {
			return fail()
		}
		if seenNames[artifact.Name] {
			return fail()
		}
		seenNames[artifact.Name] = true
		manifest.Artifacts = append(manifest.Artifacts, artifact)
	}
	for _, name := range artifactNames {
		if !seenNames[name] {
			return fail()
		}
	}
	return manifest, nil
}

// parseArtifactEntry разбирает запись артефакта строго: точный объект, ключи
// только name/size/sha256 без дублей, строки без null, размер — целое число.
func parseArtifactEntry(data []byte) (Artifact, error) {
	fail := func() (Artifact, error) { return Artifact{}, artifactFailure() }
	if len(data) == 0 || data[0] != '{' || data[len(data)-1] != '}' {
		return fail()
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return fail()
	}
	seen := map[string]bool{}
	var artifact Artifact
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || (key != "name" && key != "size" && key != "sha256") {
			return fail()
		}
		if seen[key] {
			return fail()
		}
		seen[key] = true
		var raw json.RawMessage
		if decoder.Decode(&raw) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fail()
		}
		switch key {
		case "name":
			if json.Unmarshal(raw, &artifact.Name) != nil {
				return fail()
			}
		case "sha256":
			if json.Unmarshal(raw, &artifact.SHA256) != nil {
				return fail()
			}
		case "size":
			if json.Unmarshal(raw, &artifact.Size) != nil {
				return fail()
			}
		}
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') || decoder.Decode(new(any)) != io.EOF || len(seen) != 3 {
		return fail()
	}
	if !validArtifact(artifact) {
		return fail()
	}
	return artifact, nil
}

// validArtifact: имя из закрытого списка, размер в (0, 512 MiB], строчный
// hex256 digest.
func validArtifact(artifact Artifact) bool {
	return allowedArtifactName(artifact.Name) && artifact.Size > 0 && artifact.Size <= artifactMaxSize && len(artifact.SHA256) == 64 && isLowerHex(artifact.SHA256)
}

// releaseIDDomain открывает канонический текст id набора: отделяет его от
// других sha256 и версионирует форму.
const releaseIDDomain = "brood guest release v1\n"

// ReleaseID names the Bootstrap release that m describes (#184): the
// lowercase hex sha256 of a canonical text, the line "brood guest release v1"
// followed by one line "name size sha256" per artifact in the closed name
// order. Equal artifact sets have equal IDs whatever the entry order; another
// name, size or digest gives another ID. A manifest that is not exactly the
// closed name list with valid sizes and digests has no ID.
func (m ArtifactManifest) ReleaseID() (string, error) {
	if len(m.Artifacts) != len(artifactNames) {
		return "", artifactFailure()
	}
	byName := map[string]Artifact{}
	for _, artifact := range m.Artifacts {
		if _, duplicate := byName[artifact.Name]; duplicate || !validArtifact(artifact) {
			return "", artifactFailure()
		}
		byName[artifact.Name] = artifact
	}
	digest := sha256.New()
	io.WriteString(digest, releaseIDDomain)
	for _, name := range artifactNames {
		artifact := byName[name]
		io.WriteString(digest, name+" "+strconv.FormatInt(artifact.Size, 10)+" "+artifact.SHA256+"\n")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func allowedArtifactName(name string) bool {
	for _, allowed := range artifactNames {
		if name == allowed {
			return true
		}
	}
	return false
}

func isLowerHex(text string) bool {
	for i := 0; i < len(text); i++ {
		if c := text[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ArtifactInstaller installs one Bootstrap artifact read from stream. size and
// digest (lowercase hex sha256) come from the server manifest: the installer
// reads exactly size bytes and never publishes content whose sha256 differs
// from digest.
type ArtifactInstaller func(ctx context.Context, name string, stream io.Reader, size int64, digest string) error

// ArtifactRelease installs the Bootstrap release that a verified server
// manifest describes (#184). FetchArtifacts calls it once, after the manifest
// and before any artifact request. The release downloads through download
// only the artifacts it still needs and may download none when an intact copy
// of the release is already installed.
type ArtifactRelease func(ctx context.Context, manifest ArtifactManifest, download ArtifactDownload) error

// ArtifactDownload streams one artifact of the manifest into install with the
// checks of FetchArtifacts. An artifact that is not an entry of the manifest
// is refused without a request.
type ArtifactDownload func(ctx context.Context, artifact Artifact, install ArtifactInstaller) error

// FetchArtifacts читает манифест на проверенном машинном маршруте сервера
// (без proxy и redirects, с Bearer-токеном актора) и передаёт его release до
// первого запроса артефакта (#184): что качать, решает установщик набора.
// Каждая загрузка стримит артефакт через установщик с проверкой ELF-магии и
// лимита size+1; установщик получает size и sha256 манифеста и
// сверяет digest до публикации (#265). Загрузка успешна только когда
// установщик вернулся без ошибки, получил ровно size байт и sha256 совпал.
// Причины отказов статичны: без тел и токенов.
func FetchArtifacts(parent context.Context, cfg relay.ClientConfig, release ArtifactRelease) error {
	fail := artifactFailure
	route, err := newServerRoute(cfg)
	if err != nil {
		return fail()
	}
	ctx, cancel := context.WithTimeout(parent, artifactFetchTimeout)
	defer cancel()
	manifest, err := fetchArtifactManifest(ctx, route)
	if err != nil {
		return fail()
	}
	// Загрузчик качает только записи этого манифеста: путь запроса строится
	// из имени, а сверка из size и sha256 записи.
	download := func(ctx context.Context, artifact Artifact, install ArtifactInstaller) error {
		if !slices.Contains(manifest.Artifacts, artifact) {
			return fail()
		}
		return fetchArtifact(ctx, route, artifact, install)
	}
	if err := release(ctx, manifest, download); err != nil {
		return fail()
	}
	return nil
}

// fetchArtifactManifest читает манифест машинным маршрутом с коротким таймаутом.
func fetchArtifactManifest(ctx context.Context, route serverRoute) (ArtifactManifest, error) {
	target := *route.origin
	target.Path = route.path("/api/proxmox/artifacts/")
	transport := route.transport(5 * time.Second)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return artifactFailure() }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return ArtifactManifest{}, artifactFailure()
	}
	request.Header.Set("Authorization", "Bearer "+route.token)
	response, err := client.Do(request)
	if err != nil {
		return ArtifactManifest{}, artifactFailure()
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ArtifactManifest{}, artifactFailure()
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, artifactManifestLimit+1))
	if err != nil || len(data) > artifactManifestLimit {
		return ArtifactManifest{}, artifactFailure()
	}
	return ParseArtifactManifest(data)
}

// fetchArtifact стримит один артефакт через установщика, считая digest; успех
// требует ELF-магию, ровно size байт у установщика и совпавший sha256.
func fetchArtifact(ctx context.Context, route serverRoute, artifact Artifact, install ArtifactInstaller) error {
	target := *route.origin
	target.Path = route.path("/api/proxmox/artifacts/") + "/" + artifact.Name
	transport := route.transport(30 * time.Second)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return artifactFailure() }}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return artifactFailure()
	}
	request.Header.Set("Authorization", "Bearer "+route.token)
	response, err := client.Do(request)
	if err != nil {
		return artifactFailure()
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return artifactFailure()
	}
	if response.ContentLength > 0 && response.ContentLength != artifact.Size {
		return artifactFailure()
	}
	digest := sha256.New()
	// Двойной tee: и установщик, и digest читают те же байты через io.MultiReader.
	tee := io.TeeReader(io.LimitReader(response.Body, artifact.Size+1), digest)
	// Магия ELF читается первой и через tee: digest считает её ровно один раз.
	magic := make([]byte, 4)
	if _, err := io.ReadFull(tee, magic); err != nil || string(magic) != "\x7fELF" {
		return artifactFailure()
	}
	if err := install(ctx, artifact.Name, io.MultiReader(bytes.NewReader(magic), tee), artifact.Size, artifact.SHA256); err != nil {
		return artifactFailure()
	}
	// Установщик обязан прочесть весь артефакт: добираем остаток лимита и
	// требуем, чтобы за ним не было ни байта.
	extra, err := io.Copy(io.Discard, io.LimitReader(tee, artifact.Size+1))
	if err != nil || extra != 0 {
		return artifactFailure()
	}
	want, err := hex.DecodeString(artifact.SHA256)
	if err != nil || !bytes.Equal(digest.Sum(nil), want) {
		return artifactFailure()
	}
	return nil
}
