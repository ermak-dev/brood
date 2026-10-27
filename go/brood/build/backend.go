// Docker BuildKit: публикация в проверенный реестр без запуска акторов.
// Порт brood/dev/build/backend.py (issue #35): проверка владения реестром
// и кластером вводится извне — локальная разработка и сервер дают свои.
package build

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// BackendOptions настраивает сборку: проверка владения кластером, кэш
// источников, переменные окружения docker и список разрешённых реестров.
type BackendOptions struct {
	OwnershipCheck func() error
	Cache          map[string]string
	DockerEnv      []string
	// RegistryAllowlist разрешает префиксы реестров; пустой список
	// разрешает локальный реестр разработки localhost:5002.
	RegistryAllowlist []string
}

// BuildResult — итог успешной сборки.
type BuildResult struct {
	Image   string
	Sources map[string]any
	Stdout  string
	Stderr  string
}

// BuildError хранит диагностику процесса данными для командлета.
type BuildError struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

func (e *BuildError) Error() string {
	return fmt.Sprintf("OCI image build failed with exit code %d", e.ExitCode)
}

// localRegistry — реестр локальной установки Substrate.
const localRegistry = "localhost:5002"

var patternImageDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// registryAllowed проверяет хост реестра плана по списку или локальному
// умолчанию, как сравнение split("/", 1)[0] в Python.
func (o *BackendOptions) registryAllowed(registry string) bool {
	allowlist := o.RegistryAllowlist
	if len(allowlist) == 0 {
		allowlist = []string{localRegistry}
	}
	host := strings.SplitN(registry, "/", 2)[0]
	for _, allowed := range allowlist {
		if host == strings.SplitN(strings.TrimSuffix(allowed, "/"), "/", 2)[0] {
			return true
		}
	}
	return false
}

// BackendBuild собирает образ через внешние git и docker buildx.
// digest берётся из metadata этого вызова, а не из тега.
func BackendBuild(project *Project, options BackendOptions) (*BuildResult, error) {
	plan := project.Plan
	registry, _ := plan["registry"].(string)
	if !options.registryAllowed(registry) {
		return nil, fmt.Errorf("BUILD_REGISTRY must use the owned installation registry")
	}
	if options.OwnershipCheck != nil {
		if err := options.OwnershipCheck(); err != nil {
			return nil, err
		}
	}
	root, err := os.MkdirTemp("", "brood-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	source := filepath.Join(root, "context")
	reports, err := Export(project, source, options.Cache)
	if err != nil {
		return nil, err
	}
	metadata := filepath.Join(root, "image-metadata.json")
	projectName, _ := plan["project"].(string)
	environment, _ := plan["environment"].(string)
	image := registry + "/" + projectName + "/" + environment
	random, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	tag := image + ":build-" + random
	build, _ := plan["build"].(map[string]any)
	dockerfile, _ := build["dockerfile"].(string)
	platform, _ := build["platform"].(string)
	arguments := map[string]string{}
	for name, value := range build["arguments"].(map[string]any) {
		text, _ := value.(string)
		arguments[name] = text
	}
	argv := []string{"docker", "buildx", "build", "--push", "--platform", platform,
		"--file", filepath.Join(source, dockerfile), "--tag", tag,
		"--metadata-file", metadata}
	names := sortedKeys(arguments)
	for _, name := range names {
		argv = append(argv, "--build-arg", name+"="+arguments[name])
	}
	argv = append(argv, source)
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = source
	command.Env = append(os.Environ(), options.DockerEnv...)
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if _, exit := err.(*exec.ExitError); exit {
			return nil, &BuildError{ExitCode: command.ProcessState.ExitCode(),
				Stdout: stdout.String(), Stderr: stderr.String()}
		}
		return nil, fmt.Errorf("docker is not installed")
	}
	data, err := os.ReadFile(metadata)
	if err != nil {
		return nil, fmt.Errorf("BuildKit returned no usable image metadata")
	}
	metadataValue, err := decodeOneJSON(string(data))
	if err != nil {
		return nil, fmt.Errorf("BuildKit returned no usable image metadata")
	}
	metadataObject, ok := metadataValue.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("BuildKit returned no usable image metadata")
	}
	digestValue, ok := metadataObject["containerimage.digest"]
	if !ok {
		return nil, fmt.Errorf("BuildKit returned no usable image metadata")
	}
	digest, ok := digestValue.(string)
	if !ok || !patternImageDigest.MatchString(digest) {
		return nil, fmt.Errorf("BuildKit metadata contains an invalid image digest")
	}
	return &BuildResult{Image: image + "@" + digest, Sources: reports,
		Stdout: stdout.String(), Stderr: stderr.String()}, nil
}
