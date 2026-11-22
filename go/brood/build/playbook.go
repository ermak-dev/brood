// Package build reads the project declaration from the playbook brood.yaml.
// The server builds the worker image itself: it clones the repository at a
// commit and runs that playbook with Ansible. The client reads only the
// reserved play variable brood as plain YAML and never evaluates Ansible.
package build

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"brood.dev/go/brood/gitorigin"
	"brood.dev/go/brood/projectsecret"
)

// Filename is the playbook in the repository root that declares the project
// and builds its image.
const Filename = "brood.yaml"

const (
	variable         = "brood"
	maxDocumentBytes = 1024 * 1024
	nestingLimit     = 16
	literalReason    = "project metadata must be literal: bro does not evaluate templates"
)

var (
	patternName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	// Пустая строка явно снимает ссылку на Git credential.
	patternLimit = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	patternLine  = regexp.MustCompile(`line (\d+)`)
	patternDrive = regexp.MustCompile(`^[A-Za-z]:`)
	// Имя лимита в brood.yaml и поле capacity серверной синхронизации.
	limitKeys = [][2]string{
		{"swarms", "max_swarms"},
		{"agents", "max_actors"},
		{"cpu_millicores", "max_cpu_millicores"},
		{"memory_bytes", "max_memory_bytes"},
	}
	// Ключи пьесы, непустое значение которых означает работу для образа.
	workKeys = []string{"tasks", "roles", "pre_tasks", "post_tasks", "handlers"}
)

// SecretFile declares one file delivered into the agent's home.
type SecretFile struct {
	// Path is the destination as the project secret API names it.
	Path string
	// Source is the local file, relative to the repository root.
	Source string

	line int
}

// Secrets is the brood.secrets block. It holds names and local paths only;
// values are read by ReadSecrets.
type Secrets struct {
	// Env is the local dotenv file, relative to the repository root; empty
	// when the block declares no variables.
	Env string
	// Files are the declared secret files in declaration order.
	Files []SecretFile

	envLine int
}

// Project is a validated declaration from brood.yaml.
type Project struct {
	// Root is the directory holding brood.yaml; empty after Parse.
	Root string
	// Name is the project name.
	Name string
	// Config is the declaration accepted by the server's project sync.
	Config map[string]any
	// HasTasks reports that some play has work to do for the image.
	HasTasks bool
	// Secrets is nil when brood.yaml has no brood.secrets block; a present
	// empty block is a non-nil empty value.
	Secrets *Secrets
	// RepositoryResolved distinguishes an explicit repository, including the
	// empty one that disables cloning, from a missing Git origin.
	RepositoryResolved bool
}

// failure строит отказ «файл: поле (строка): причина» без трассировки.
func failure(field string, line int, reason string) error {
	location := Filename + ": " + field
	if line > 0 {
		location += fmt.Sprintf(" (line %d)", line)
	}
	return fmt.Errorf("%s: %s", location, reason)
}

// hasControl ищет управляющие символы, включая диапазон C1.
func hasControl(value string) bool {
	for _, char := range value {
		if char < 32 || (char >= 127 && char <= 159) {
			return true
		}
	}
	return false
}

func templated(value string) bool {
	return strings.Contains(value, "{{") || strings.Contains(value, "{%")
}

// checkLiteral пропускает под brood только строки, целые, null, mapping и
// последовательности из них: шаблоны, теги, якоря и дубликаты — отказ.
func checkLiteral(node *yaml.Node, field string, depth int) error {
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return failure(field, node.Line, "YAML anchors and aliases are forbidden; "+literalReason)
	}
	if node.Style&yaml.TaggedStyle != 0 {
		return failure(field, node.Line, "YAML tag "+printable(node.Tag)+" is forbidden; "+literalReason)
	}
	if depth >= nestingLimit {
		return failure(field, node.Line, "nesting limit exceeded")
	}
	switch node.Kind {
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.ShortTag() != "!!str" || key.Style&yaml.TaggedStyle != 0 || key.Anchor != "" {
				return failure(field, key.Line, "mapping keys must be plain strings")
			}
			if key.Value == "<<" {
				return failure(field, key.Line, "YAML merge keys are forbidden")
			}
			if seen[key.Value] {
				return failure(join(field, key.Value), key.Line, "duplicate mapping key")
			}
			seen[key.Value] = true
			if err := checkLiteral(node.Content[i+1], join(field, key.Value), depth+1); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, item := range node.Content {
			if err := checkLiteral(item, fmt.Sprintf("%s[%d]", field, i), depth+1); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		switch node.ShortTag() {
		case "!!null", "!!int":
		case "!!str":
			if templated(node.Value) {
				return failure(field, node.Line, "templates are forbidden; "+literalReason)
			}
		default:
			return failure(field, node.Line, "expected a string, an integer or null; "+literalReason)
		}
	default:
		return failure(field, node.Line, "unsupported YAML node")
	}
	return nil
}

// printable сокращает и обезвреживает чужой текст для сообщения об отказе.
func printable(value string) string {
	if len(value) > 32 || hasControl(value) || value == "" {
		return "[tag]"
	}
	return value
}

// join собирает имя поля, скрывая нестандартные ключи.
func join(field, key string) string {
	if len(key) > 64 || hasControl(key) || strings.ContainsAny(key, " \t") || key == "" {
		key = "[field]"
	}
	return field + "." + key
}

// fields раскладывает mapping по именам, отвергая поля вне allowed.
func fields(node *yaml.Node, field string, allowed ...string) (map[string]*yaml.Node, error) {
	if node.Kind != yaml.MappingNode {
		return nil, failure(field, node.Line, "expected a mapping")
	}
	result := map[string]*yaml.Node{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		known := false
		for _, name := range allowed {
			known = known || name == key.Value
		}
		if !known {
			return nil, failure(join(field, key.Value), key.Line, "unknown field; expected one of: "+strings.Join(allowed, ", "))
		}
		result[key.Value] = node.Content[i+1]
	}
	return result, nil
}

// text читает строковый скаляр длиной до limit символов.
func text(node *yaml.Node, field string, limit int) (string, error) {
	if node.Kind != yaml.ScalarNode || node.ShortTag() != "!!str" {
		return "", failure(field, node.Line, "expected a string")
	}
	if utf8.RuneCountInString(node.Value) > limit {
		return "", failure(field, node.Line, fmt.Sprintf("expected at most %d characters", limit))
	}
	return node.Value, nil
}

func isNull(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.ShortTag() == "!!null"
}

// resolve раскрывает alias вне brood: остальной плейбук читается нестрого.
func resolve(node *yaml.Node) *yaml.Node {
	for depth := 0; node != nil && node.Kind == yaml.AliasNode && depth < nestingLimit; depth++ {
		node = node.Alias
	}
	return node
}

// Parse validates the text of brood.yaml. The repository stays as declared:
// an absent brood.repository is resolved from the Git origin only by Load.
func Parse(source string) (*Project, error) {
	if len(source) > maxDocumentBytes {
		return nil, failure("document", 1, "document exceeds 1 MiB")
	}
	decoder := yaml.NewDecoder(strings.NewReader(source))
	var document, extra yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if err == io.EOF {
			return nil, failure("document", 1, "empty document")
		}
		line := 1
		if match := patternLine.FindStringSubmatch(err.Error()); match != nil {
			line, _ = strconv.Atoi(match[1])
		}
		return nil, failure("document", line, "invalid YAML")
	}
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, failure("document", 1, "expected a single valid YAML document")
	}
	if len(document.Content) == 0 {
		return nil, failure("document", 1, "empty document")
	}
	plays := resolve(document.Content[0])
	if plays.Kind != yaml.SequenceNode {
		return nil, failure("document", plays.Line, "expected an Ansible playbook: a YAML sequence of plays")
	}
	var declarations []*yaml.Node
	hasTasks := false
	for _, item := range plays.Content {
		play := resolve(item)
		if play == nil || play.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(play.Content); i += 2 {
			key, value := play.Content[i].Value, resolve(play.Content[i+1])
			if value == nil {
				continue
			}
			switch {
			case key == "import_playbook":
				hasTasks = true
			case key == "vars" && value.Kind == yaml.MappingNode:
				for j := 0; j+1 < len(value.Content); j += 2 {
					if value.Content[j].Value == variable {
						declarations = append(declarations, value.Content[j+1])
					}
				}
			default:
				for _, work := range workKeys {
					if key == work && !isNull(value) && (value.Kind == yaml.ScalarNode || len(value.Content) > 0) {
						hasTasks = true
					}
				}
			}
		}
	}
	switch len(declarations) {
	case 0:
		return nil, failure(variable, 0, "no play defines vars.brood; exactly one play of "+Filename+" must define it")
	case 1:
	default:
		lines := make([]string, len(declarations))
		for i, node := range declarations {
			lines[i] = strconv.Itoa(node.Line)
		}
		return nil, failure(variable, declarations[1].Line, "vars.brood is defined "+strconv.Itoa(len(declarations))+" times (lines "+strings.Join(lines, ", ")+"); exactly one play of "+Filename+" must define it")
	}
	declaration := declarations[0]
	if err := checkLiteral(declaration, variable, 0); err != nil {
		return nil, err
	}
	project := &Project{HasTasks: hasTasks}
	if err := project.parse(declaration); err != nil {
		return nil, err
	}
	return project, nil
}

// parse сохраняет присутствие необязательных полей серверного проекта.
func (p *Project) parse(node *yaml.Node) error {
	block, err := fields(node, variable, "project", "description", "repository", "limits", "secrets")
	if err != nil {
		return err
	}
	if block["project"] == nil {
		return failure("brood.project", node.Line, "required field is missing")
	}
	if p.Name, err = text(block["project"], "brood.project", 63); err != nil {
		return err
	}
	if !patternName.MatchString(p.Name) {
		return failure("brood.project", block["project"].Line, "expected lowercase letters, digits and inner hyphens")
	}
	p.Config = map[string]any{"name": p.Name, "repository": ""}
	repository := ""
	if node := block["repository"]; node != nil {
		if repository, err = text(node, "brood.repository", gitorigin.MaxLength); err != nil {
			return err
		}
		p.RepositoryResolved = true
		if repository != "" {
			canonical, err := gitorigin.FromRemote(repository)
			if err != nil {
				return failure("brood.repository", node.Line, err.Error())
			}
			p.Config["repository"] = canonical
		}
	}
	if node := block["description"]; node != nil {
		description, err := text(node, "brood.description", 4096)
		if err != nil {
			return err
		}
		if hasControl(description) {
			return failure("brood.description", node.Line, "control characters are forbidden")
		}
		p.Config["description"] = description
	}
	if node := block["limits"]; node != nil {
		capacity, err := parseLimits(node)
		if err != nil {
			return err
		}
		p.Config["capacity"] = capacity
	}
	if node := block["secrets"]; node != nil {
		if p.Secrets, err = parseSecrets(node); err != nil {
			return err
		}
	}
	return nil
}

// parseLimits переводит brood.limits в capacity сервера. Присутствующий блок
// объявляет все четыре лимита: пропущенный и null — без ограничения.
func parseLimits(node *yaml.Node) (map[string]any, error) {
	capacity := map[string]any{}
	for _, pair := range limitKeys {
		capacity[pair[1]] = nil
	}
	if isNull(node) {
		return capacity, nil
	}
	names := make([]string, len(limitKeys))
	for i, pair := range limitKeys {
		names[i] = pair[0]
	}
	limits, err := fields(node, "brood.limits", names...)
	if err != nil {
		return nil, err
	}
	for _, pair := range limitKeys {
		limit := limits[pair[0]]
		if limit == nil || isNull(limit) {
			continue
		}
		field := "brood.limits." + pair[0]
		reason := "expected null or a nonnegative decimal integer within int64"
		if limit.Kind != yaml.ScalarNode || limit.ShortTag() != "!!int" || !patternLimit.MatchString(limit.Value) {
			return nil, failure(field, limit.Line, reason)
		}
		if _, err := strconv.ParseInt(limit.Value, 10, 64); err != nil {
			return nil, failure(field, limit.Line, reason)
		}
		capacity[pair[1]] = limit.Value
	}
	return capacity, nil
}

func parseSecrets(node *yaml.Node) (*Secrets, error) {
	if isNull(node) {
		return nil, failure("brood.secrets", node.Line, "expected a mapping; write {} to clear the project secrets or remove the key to leave them untouched")
	}
	block, err := fields(node, "brood.secrets", "env", "files")
	if err != nil {
		return nil, err
	}
	secrets := &Secrets{}
	if node := block["env"]; node != nil {
		if secrets.Env, err = text(node, "brood.secrets.env", 1024); err != nil {
			return nil, err
		}
		if reason := sourceFault(secrets.Env); reason != "" {
			return nil, failure("brood.secrets.env", node.Line, reason)
		}
		secrets.envLine = node.Line
	}
	if node := block["files"]; node != nil && !isNull(node) {
		if node.Kind != yaml.SequenceNode {
			return nil, failure("brood.secrets.files", node.Line, "expected a sequence")
		}
		if len(node.Content) > projectsecret.MaxFiles {
			return nil, failure("brood.secrets.files", node.Line, fmt.Sprintf("expected at most %d files", projectsecret.MaxFiles))
		}
		seen := map[string]bool{}
		for i, item := range node.Content {
			field := fmt.Sprintf("brood.secrets.files[%d]", i)
			entry, err := fields(item, field, "path", "source")
			if err != nil {
				return nil, err
			}
			for _, required := range []string{"path", "source"} {
				if entry[required] == nil {
					return nil, failure(field+"."+required, item.Line, "required field is missing")
				}
			}
			file := SecretFile{line: entry["source"].Line}
			if file.Path, err = text(entry["path"], field+".path", 1024); err != nil {
				return nil, err
			}
			if err := projectsecret.ValidateFileName(file.Path); err != nil {
				return nil, failure(field+".path", entry["path"].Line, err.Error())
			}
			if seen[file.Path] {
				return nil, failure(field+".path", entry["path"].Line, "duplicate secret file path")
			}
			seen[file.Path] = true
			if file.Source, err = text(entry["source"], field+".source", 1024); err != nil {
				return nil, err
			}
			if reason := sourceFault(file.Source); reason != "" {
				return nil, failure(field+".source", entry["source"].Line, reason)
			}
			secrets.Files = append(secrets.Files, file)
		}
	}
	return secrets, nil
}

// sourceFault называет нарушение правила локального пути или возвращает "".
func sourceFault(value string) string {
	switch {
	case value == "":
		return "expected a file path relative to the repository root"
	case hasControl(value) || strings.Contains(value, `\`):
		return "control characters and backslashes are forbidden"
	case strings.HasPrefix(value, "/") || patternDrive.MatchString(value):
		return "absolute paths are forbidden; expected a path relative to the repository root"
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return `".." segments are forbidden; the file must be inside the repository`
		}
	}
	if path.Clean(value) != value {
		return "expected a clean path without empty or dot segments"
	}
	return ""
}

// Load reads directory/brood.yaml. An absent brood.repository becomes the
// Git origin of that checkout, normalized without changing its transport; Git itself is optional.
func Load(directory string) (*Project, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if absolute, err = filepath.EvalSymlinks(absolute); err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Join(absolute, Filename))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s is not found in the current directory; run from the repository root", Filename)
		}
		return nil, fmt.Errorf("could not read %s; it is inaccessible", Filename)
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", Filename)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("could not read %s", Filename)
	}
	project, err := Parse(string(data))
	if err != nil {
		return nil, err
	}
	project.Root = absolute
	if project.RepositoryResolved {
		return project, nil
	}
	remote, present, err := origin(absolute)
	if err != nil || !present {
		return project, err
	}
	canonical, err := gitorigin.FromRemote(remote)
	if err != nil {
		return nil, fmt.Errorf("%s: brood.repository: Git origin: %w", Filename, err)
	}
	project.Config["repository"] = canonical
	project.RepositoryResolved = true
	return project, nil
}
