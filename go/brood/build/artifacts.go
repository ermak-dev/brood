// Независимые манифесты сборки и атомарная публикация полного комплекта.
// Порт brood/dev/build/artifacts.py (issue #35).
package build

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const (
	artifactOwner = "brood.dev/build/v1"
)

// artifactFiles — точный состав комплекта.
var artifactFiles = map[string]bool{
	"actor-template.json": true, "egress-policy.json": true, "build.json": true,
}

var (
	patternDNSLabel63  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	patternRunName     = regexp.MustCompile(`^run-[0-9a-f]{32}$`)
	patternDigestImage = regexp.MustCompile(`^[0-9a-f]{64}$`)
	patternUint64      = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// artifactLabel требует метку-идентификатор комплекта.
func artifactLabel(value any) (string, error) {
	text, ok := value.(string)
	if !ok || !patternDNSLabel63.MatchString(text) {
		return "", fmt.Errorf("artifact identity must be a lowercase DNS label of at most 63 characters")
	}
	return text, nil
}

// canonicalArtifactJSON кодирует как json.dumps(sort_keys, separators, ensure_ascii).
func canonicalArtifactJSON(value any) (string, error) {
	var builder strings.Builder
	if err := writeCanonicalJSON(&builder, value); err != nil {
		return "", err
	}
	return builder.String(), nil
}

// writeCanonicalJSON печатает компактный канонический JSON.
func writeCanonicalJSON(builder *strings.Builder, value any) error {
	switch typed := value.(type) {
	case nil:
		builder.WriteString("null")
	case bool:
		if typed {
			builder.WriteString("true")
		} else {
			builder.WriteString("false")
		}
	case string:
		encoded, err := pythonJSONString(typed)
		if err != nil {
			return err
		}
		builder.WriteString(encoded)
	case float64:
		text, err := pythonJSONNumber(typed)
		if err != nil {
			return err
		}
		builder.WriteString(text)
	case int:
		builder.WriteString(strconv.Itoa(typed))
	case int64:
		builder.WriteString(strconv.FormatInt(typed, 10))
	case uint64:
		builder.WriteString(strconv.FormatUint(typed, 10))
	case []any:
		builder.WriteString("[")
		for index, item := range typed {
			if index > 0 {
				builder.WriteString(",")
			}
			if err := writeCanonicalJSON(builder, item); err != nil {
				return err
			}
		}
		builder.WriteString("]")
	case map[string]any:
		builder.WriteString("{")
		for index, key := range sortedKeys(typed) {
			if index > 0 {
				builder.WriteString(",")
			}
			encoded, err := pythonJSONString(key)
			if err != nil {
				return err
			}
			builder.WriteString(encoded + ":")
			if err := writeCanonicalJSON(builder, typed[key]); err != nil {
				return err
			}
		}
		builder.WriteString("}")
	default:
		return fmt.Errorf("artifact bundle must contain valid JSON objects")
	}
	return nil
}

// planSourcesForArtifacts собирает белые поля источников и проверяет отчёты.
func planSourcesForArtifacts(plan map[string]any, reports map[string]any) (map[string]any, error) {
	planSources, _ := plan["sources"].(map[string]any)
	if reports == nil {
		reports = map[string]any{}
	}
	for name := range reports {
		if _, ok := planSources[name]; !ok {
			return nil, fmt.Errorf("source reports must name selected sources")
		}
	}
	result := map[string]any{}
	for _, name := range sortedKeys(planSources) {
		source, _ := planSources[name].(map[string]any)
		// Копируются только декларативные поля, никогда пути checkout или config.
		item := map[string]any{}
		for _, key := range []string{"type", "destination", "gitMetadata", "repository", "revision"} {
			if value, ok := source[key]; ok {
				item[key] = deepCopyValue(value)
			}
		}
		raw, ok := reports[name]
		if !ok {
			raw = map[string]any{}
		}
		report, isMap := raw.(map[string]any)
		if !isMap {
			return nil, fmt.Errorf("source report must be an object")
		}
		for _, key := range []string{"repository", "revision", "workingTree", "dirty"} {
			if value, ok := report[key]; ok {
				if key == "workingTree" || key == "dirty" {
					if _, ok := value.(bool); !ok {
						return nil, fmt.Errorf("working source provenance flags must be booleans")
					}
				}
				if planned, exists := item[key]; exists && !valuesEqual(value, planned) {
					return nil, fmt.Errorf("source report does not match planned input")
				}
				item[key] = value
			}
		}
		if repositoryValue, ok := item["repository"]; ok {
			text, _ := repositoryValue.(string)
			if _, err := Repository(text); err != nil {
				return nil, err
			}
		}
		revision, ok := item["revision"]
		if ok {
			text, isString := revision.(string)
			if !isString || !patternHex40.MatchString(text) {
				return nil, fmt.Errorf("source revision must be a full lowercase 40-hex commit")
			}
		}
		result[name] = item
	}
	return result, nil
}

// Render возвращает три JSON-объекта без изменения плана и без побочных эффектов.
func Render(plan map[string]any, image string, sourceReports map[string]any) (map[string]any, error) {
	identity := map[string]any{}
	for _, key := range []string{"project", "target", "environment", "atespace"} {
		label, err := artifactLabel(plan[key])
		if err != nil {
			return nil, err
		}
		identity[key] = label
	}
	registry, _ := plan["registry"].(string)
	project, _ := plan["project"].(string)
	environment, _ := plan["environment"].(string)
	prefix := registry + "/" + project + "/" + environment + "@sha256:"
	if !strings.HasPrefix(image, prefix) || !patternDigestImage.MatchString(image[len(prefix):]) {
		return nil, fmt.Errorf("image must match the planned repository with a sha256 digest")
	}
	actor, _ := deepCopyValue(plan["actor"]).(map[string]any)
	limits, _ := actor["resources"].(map[string]any)["limits"].(map[string]any)
	for _, name := range sortedKeys(limits) {
		text, ok := limits[name].(string)
		if !ok || !patternUint64.MatchString(text) || len(text) > 20 {
			return nil, fmt.Errorf("actor resource must be a positive uint64 string")
		}
		if number, err := strconv.ParseUint(text, 10, 64); err != nil || number == 0 {
			return nil, fmt.Errorf("actor resource must be a positive uint64 string")
		}
	}
	containers, _ := actor["containers"].([]any)
	if len(containers) == 0 {
		return nil, fmt.Errorf("actor template must declare a container")
	}
	containers[0].(map[string]any)["image"] = image
	snapshotConfig, ok := actor["snapshotConfig"].(map[string]any)
	if !ok {
		snapshotConfig = map[string]any{}
		actor["snapshotConfig"] = snapshotConfig
	}
	if _, ok := snapshotConfig["goldenSnapshotConfig"]; !ok {
		snapshotConfig["goldenSnapshotConfig"] = map[string]any{"mode": "GOLDEN_SNAPSHOT_MODE_DISABLED"}
	}
	canonical, err := canonicalArtifactJSON(map[string]any{
		"actor": actor, "network": deepCopyValue(plan["network"]), "identity": identity,
	})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(canonical))
	hexDigest := hex.EncodeToString(digest[:])
	combined := project + "-" + identity["target"].(string)
	if len(combined) > 38 {
		combined = combined[:38]
	}
	stem := strings.TrimRight(combined, "-")
	name := stem + "-" + hexDigest[:24]
	actor["metadata"] = map[string]any{"atespace": plan["atespace"], "name": name}
	network, _ := plan["network"].(map[string]any)
	rules := []any{}
	for _, item := range []struct{ source, protocol string }{{"tls", "tlsPassthrough"}, {"http", "http"}} {
		rule, ok := network[item.source].(map[string]any)
		if !ok {
			continue
		}
		rules = append(rules, map[string]any{item.protocol: map[string]any{
			"hostnames": deepCopyValue(rule["hosts"]),
			"ports":     map[string]any{"numbers": deepCopyValue(rule["ports"])},
		}})
	}
	sources, err := planSourcesForArtifacts(plan, sourceReports)
	if err != nil {
		return nil, err
	}
	provenance := map[string]any{
		"apiVersion": deepCopyValue(plan["apiVersion"]), "builder": artifactOwner,
	}
	for _, key := range sortedKeys(identity) {
		provenance[key] = identity[key]
	}
	provenance["output"] = map[string]any{"owner": artifactOwner, "target": plan["target"]}
	provenance["sources"] = sources
	provenance["build"] = deepCopyValue(plan["build"])
	provenance["pins"] = deepCopyValue(plan["pins"])
	provenance["image"] = image
	provenance["imageDigest"] = "sha256:" + image[len(prefix):]
	provenance["template"] = name
	provenance["templateDigest"] = "sha256:" + hexDigest
	return map[string]any{
		"actor-template.json": actor,
		"egress-policy.json": map[string]any{
			"metadata": map[string]any{"atespace": plan["atespace"], "name": "default"},
			"rules":    rules,
		},
		"build.json": provenance,
	}, nil
}

// directoryChain проверяет родителей пути: каталоги, не ссылки и не файлы.
// Как _directory: не resolve(), он скрыл бы symlink, который требуется отвергнуть.
// Отказы окружения уже переведены в сообщение публикации, как except OSError.
func directoryChain(path string, create bool) error {
	var chain []string
	for current := path; ; current = filepath.Dir(current) {
		chain = append(chain, current)
		if filepath.Dir(current) == current {
			break
		}
	}
	for index := len(chain) - 1; index >= 0; index-- {
		parent := chain[index]
		info, err := os.Lstat(parent)
		if err != nil {
			if !os.IsNotExist(err) {
				return publishRefusal(err)
			}
			if !create {
				return fmt.Errorf("artifact parent directory does not exist")
			}
			if err := os.Mkdir(parent, 0o755); err != nil && !os.IsExist(err) {
				return publishRefusal(err)
			}
			info, err = os.Lstat(parent)
			if err != nil {
				return publishRefusal(err)
			}
		}
		if !info.IsDir() {
			return fmt.Errorf("artifact parent must be a directory, not a symlink or file")
		}
	}
	return nil
}

// artifactMarker проверяет метку владельца в build.json.
func artifactMarker(value any, target string) bool {
	mapping, ok := value.(map[string]any)
	if !ok {
		return false
	}
	output, ok := mapping["output"].(map[string]any)
	return mapping["builder"] == artifactOwner && mapping["target"] == target &&
		len(output) == 2 && output["owner"] == artifactOwner && output["target"] == target
}

// ownedRun проверяет существующую ссылку output и её цель.
func ownedRun(output, runs, target string) error {
	info, err := os.Lstat(output)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("refusing foreign artifact output; expected an owned symlink")
	}
	link, err := os.Readlink(output)
	if err != nil {
		return err
	}
	parts := strings.Split(filepath.ToSlash(link), "/")
	if filepath.IsAbs(link) || len(parts) != 3 || parts[0] != ".runs" || parts[1] != target ||
		!patternRunName.MatchString(parts[2]) {
		return fmt.Errorf("refusing foreign artifact output symlink")
	}
	run := filepath.Join(runs, parts[2])
	runInfo, err := os.Lstat(run)
	if err != nil || !runInfo.IsDir() {
		return fmt.Errorf("refusing artifact output without an owned complete run")
	}
	entries, err := os.ReadDir(run)
	if err != nil {
		return fmt.Errorf("refusing artifact output without an owned complete run")
	}
	if len(entries) != len(artifactFiles) && len(entries) != len(artifactFiles)+1 {
		return fmt.Errorf("refusing artifact output without an owned complete run")
	}
	count := 0
	for _, entry := range entries {
		if (!artifactFiles[entry.Name()] && entry.Name() != "image.oci.tar") || !entry.Type().IsRegular() {
			return fmt.Errorf("refusing artifact output without an owned complete run")
		}
		if artifactFiles[entry.Name()] {
			count++
		}
	}
	if count != len(artifactFiles) {
		return fmt.Errorf("refusing artifact output without an owned complete run")
	}
	data, err := os.ReadFile(filepath.Join(run, "build.json"))
	if err != nil {
		return fmt.Errorf("refusing artifact output without an owned complete run")
	}
	decoded, err := decodeOneJSON(string(data))
	if err != nil {
		return fmt.Errorf("refusing artifact output without an owned complete run")
	}
	if !artifactMarker(decoded, target) {
		return fmt.Errorf("refusing artifact output without an owned complete run")
	}
	return nil
}

// encodedBundle проверяет комплект и кодирует его файлы.
func encodedBundle(artifacts map[string]any, target string) (map[string]string, error) {
	if len(artifacts) != len(artifactFiles) {
		return nil, fmt.Errorf("expected a complete owned artifact bundle for this target")
	}
	for name, value := range artifacts {
		if !artifactFiles[name] {
			return nil, fmt.Errorf("expected a complete owned artifact bundle for this target")
		}
		if _, ok := value.(map[string]any); !ok {
			return nil, fmt.Errorf("expected a complete owned artifact bundle for this target")
		}
	}
	if !artifactMarker(artifacts["build.json"], target) {
		return nil, fmt.Errorf("expected a complete owned artifact bundle for this target")
	}
	result := map[string]string{}
	for name, value := range artifacts {
		text, err := canonicalArtifactJSON(value)
		if err != nil {
			return nil, fmt.Errorf("artifact bundle must contain valid JSON objects")
		}
		result[name] = text + "\n"
	}
	return result, nil
}

// randomHex выдает четное число случайных байтов hex-строкой.
func randomHex(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// publishRefusal превращает отказ каталога в сообщение Python: для службы
// нет разницы, какая именно операция с файлами не удалась.
func publishRefusal(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("could not publish artifact bundle; previous output preserved")
}

// Publish публикует новый комплект, сохраняя прежнюю ссылку при любом отказе.
func Publish(projectRoot, target string, artifacts map[string]any) (string, error) {
	return publishBundle(projectRoot, target, artifacts, "")
}

// PublishLocal atomically publishes the normal artifacts and image.oci.tar.
// The caller's archive is preserved; existing owned outputs remain on failure.
func PublishLocal(projectRoot, target string, artifacts map[string]any, archivePath string) (string, error) {
	if err := ValidateOCIArchive(archivePath); err != nil {
		return "", err
	}
	return publishBundle(projectRoot, target, artifacts, archivePath)
}

func publishBundle(projectRoot, target string, artifacts map[string]any, archivePath string) (string, error) {
	if _, err := artifactLabel(target); err != nil {
		return "", err
	}
	encoded, err := encodedBundle(artifacts, target)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", err
	}
	output := filepath.Join(root, ".brood", "build", target)
	runs := filepath.Join(root, ".brood", "build", ".runs", target)
	if err := directoryChain(root, false); err != nil {
		return "", err
	}
	if err := directoryChain(filepath.Dir(output), true); err != nil {
		return "", err
	}
	// Чужой output отвергается до создания служебного каталога цели.
	if info, err := os.Lstat(output); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return "", fmt.Errorf("refusing foreign artifact output")
	}
	if err := directoryChain(runs, true); err != nil {
		return "", err
	}
	// O_NOFOLLOW как в Python: каталог блокировки не может быть ссылкой.
	lock, err := os.OpenFile(runs, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", publishIOFailure()
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", publishIOFailure()
	}
	if err := ownedRun(output, runs, target); err != nil {
		return "", err
	}
	identifier, err := randomHex(16)
	if err != nil {
		return "", publishIOFailure()
	}
	run := filepath.Join(runs, "run-"+identifier)
	if err := os.Mkdir(run, 0o755); err != nil {
		return "", publishIOFailure()
	}
	names := make([]string, 0, len(encoded))
	for name := range encoded {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		destination := filepath.Join(run, name)
		file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return "", preserveRun(run)
		}
		if _, err := file.WriteString(encoded[name]); err != nil {
			file.Close()
			return "", preserveRun(run)
		}
		if err := file.Sync(); err != nil {
			file.Close()
			return "", preserveRun(run)
		}
		if err := file.Close(); err != nil {
			return "", preserveRun(run)
		}
	}
	if archivePath != "" {
		input, err := os.OpenFile(archivePath, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return "", preserveRun(run)
		}
		info, statErr := input.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			input.Close()
			return "", preserveRun(run)
		}
		file, err := os.OpenFile(filepath.Join(run, "image.oci.tar"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			input.Close()
			return "", preserveRun(run)
		}
		_, copyErr := io.Copy(file, input)
		input.Close()
		syncErr := file.Sync()
		closeErr := file.Close()
		if copyErr != nil || syncErr != nil || closeErr != nil {
			return "", preserveRun(run)
		}
		if err := ValidateOCIArchive(filepath.Join(run, "image.oci.tar")); err != nil {
			return "", preserveRun(run)
		}
	}
	runDir, err := os.Open(run)
	if err != nil {
		return "", preserveRun(run)
	}
	if err := runDir.Sync(); err != nil {
		runDir.Close()
		return "", preserveRun(run)
	}
	runDir.Close()
	// Уникальная временная ссылка не задевает существующие файлы соседей.
	pointer := filepath.Join(filepath.Dir(output), ".pointer-"+identifier)
	if err := os.Symlink(filepath.Join(".runs", target, "run-"+identifier), pointer); err != nil {
		return "", preserveRun(run)
	}
	if err := directoryChain(runs, false); err != nil {
		os.Remove(pointer)
		return "", preserveRun(run)
	}
	if err := ownedRun(output, runs, target); err != nil {
		os.Remove(pointer)
		return "", preserveRun(run)
	}
	if err := os.Rename(pointer, output); err != nil {
		os.Remove(pointer)
		return "", preserveRun(run)
	}
	return output, nil
}

// preserveRun убирает неудавшийся run и переводит отказ в сообщение публикации.
func preserveRun(run string) error {
	os.RemoveAll(run)
	return fmt.Errorf("could not publish artifact bundle; previous output preserved")
}

// publishIOFailure переводит отказы окружения в сообщение публикации.
func publishIOFailure() error {
	return fmt.Errorf("could not publish artifact bundle; previous output preserved")
}
