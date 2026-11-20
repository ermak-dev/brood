// Package build reads the project declaration from a Broodfile. The server
// builds the worker image itself: it clones the repository at a commit and
// runs the declared Ansible playbook, so the client only declares the project
// and names the playbook.
package build

import (
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
)

// APIVersion is the only Broodfile schema version this client reads.
const APIVersion = "brood.dev/v1alpha2"

const (
	filename         = "Broodfile"
	maxDocumentBytes = 1024 * 1024
	nestingLimit     = 16
)

var (
	patternName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	// Пустая строка явно снимает ссылку на Git credential.
	patternOptionalName = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)?$`)
	patternLimit        = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
	patternLine         = regexp.MustCompile(`line (\d+)`)
	patternDrive        = regexp.MustCompile(`^[A-Za-z]:`)
	capacityKeys        = []string{"max_swarms", "max_actors", "max_cpu_millicores", "max_memory_bytes"}
)

// Image is the image block of a Broodfile.
type Image struct {
	// Playbook is the Ansible playbook path inside the repository.
	Playbook string `json:"playbook"`
}

// Project is a validated Broodfile.
type Project struct {
	// Root is the directory holding the Broodfile; empty after Parse.
	Root string
	// Name is the project name.
	Name string
	// Config is the declaration accepted by the server's project sync.
	Config map[string]any
	// Image is nil when the Broodfile declares no image.
	Image *Image
	// RepositoryResolved distinguishes an explicit repository, including the
	// empty one that disables cloning, from a missing Git origin.
	RepositoryResolved bool
}

// Plan returns the resolved declaration as printed by bro build --plan.
func (p *Project) Plan() map[string]any {
	return map[string]any{"apiVersion": APIVersion, "projectConfig": p.Config, "image": p.Image}
}

// failure строит отказ «файл: поле (строка): причина» без трассировки.
func failure(field string, line int, reason string) error {
	location := filename + ": " + field
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

// checkNodes отвергает якоря, теги, merge-ключи, дубликаты и глубину.
func checkNodes(node *yaml.Node, field string, depth int) error {
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return failure("document", node.Line, "YAML anchors and aliases are forbidden")
	}
	if depth >= nestingLimit {
		return failure("document", node.Line, "nesting limit exceeded")
	}
	switch node.Tag {
	case "!!null", "!!bool", "!!int", "!!float", "!!str", "!!seq", "!!map":
	default:
		return failure(field, node.Line, "unsupported YAML tag")
	}
	switch node.Kind {
	case yaml.MappingNode:
		seen := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return failure(field, key.Line, "mapping keys must be strings")
			}
			if key.Value == "<<" {
				return failure(field, key.Line, "YAML merge keys are forbidden")
			}
			if seen[key.Value] {
				return failure(join(field, key.Value), key.Line, "duplicate mapping key")
			}
			seen[key.Value] = true
			if err := checkNodes(node.Content[i+1], join(field, key.Value), depth+1); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for _, item := range node.Content {
			if err := checkNodes(item, field, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// join собирает имя поля, скрывая нестандартные ключи.
func join(field, key string) string {
	if len(key) > 64 || hasControl(key) || strings.ContainsAny(key, " \t") || key == "" {
		key = "[field]"
	}
	if field == "document" {
		return key
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
			return nil, failure(join(field, key.Value), key.Line, "unknown field")
		}
		result[key.Value] = node.Content[i+1]
	}
	return result, nil
}

// text читает строковый скаляр длиной до limit символов.
func text(node *yaml.Node, field string, limit int) (string, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
		return "", failure(field, node.Line, "expected a string")
	}
	if utf8.RuneCountInString(node.Value) > limit {
		return "", failure(field, node.Line, fmt.Sprintf("expected at most %d characters", limit))
	}
	return node.Value, nil
}

// Parse validates Broodfile text. The repository stays as declared: an absent
// project.repository is resolved from the Git origin only by Load.
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
		return nil, failure("document", 1, "invalid YAML")
	}
	if len(document.Content) == 0 {
		return nil, failure("document", 1, "empty document")
	}
	root := document.Content[0]
	if err := checkNodes(root, "document", 0); err != nil {
		return nil, err
	}
	top, err := fields(root, "document", "apiVersion", "name", "project", "image")
	if err != nil {
		return nil, err
	}
	for _, required := range []string{"apiVersion", "name"} {
		if top[required] == nil {
			return nil, failure(required, root.Line, "required field is missing")
		}
	}
	version, err := text(top["apiVersion"], "apiVersion", 64)
	if err != nil {
		return nil, err
	}
	if version != APIVersion {
		return nil, failure("apiVersion", top["apiVersion"].Line, "expected "+APIVersion)
	}
	name, err := text(top["name"], "name", 63)
	if err != nil {
		return nil, err
	}
	if !patternName.MatchString(name) {
		return nil, failure("name", top["name"].Line, "expected lowercase letters, digits and inner hyphens")
	}
	project := &Project{Name: name, Config: map[string]any{"name": name, "repository": ""}}
	if node := top["project"]; node != nil {
		if err := project.parseProject(node); err != nil {
			return nil, err
		}
	}
	if node := top["image"]; node != nil {
		if project.Image, err = parseImage(node); err != nil {
			return nil, err
		}
	}
	return project, nil
}

// parseProject сохраняет присутствие необязательных полей серверного проекта.
func (p *Project) parseProject(node *yaml.Node) error {
	block, err := fields(node, "project", "description", "repository", "git_credential", "capacity")
	if err != nil {
		return err
	}
	repository, explicit := "", false
	if node := block["repository"]; node != nil {
		if repository, err = text(node, "project.repository", gitorigin.MaxLength); err != nil {
			return err
		}
		explicit, p.RepositoryResolved = true, true
		if repository != "" {
			canonical, err := gitorigin.FromRemote(repository)
			if err != nil {
				return failure("project.repository", node.Line, err.Error())
			}
			p.Config["repository"] = canonical
		}
	}
	if node := block["description"]; node != nil {
		description, err := text(node, "project.description", 4096)
		if err != nil {
			return err
		}
		if hasControl(description) {
			return failure("project.description", node.Line, "control characters are forbidden")
		}
		p.Config["description"] = description
	}
	if node := block["capacity"]; node != nil {
		limits, err := fields(node, "project.capacity", capacityKeys...)
		if err != nil {
			return err
		}
		capacity := map[string]any{}
		for key, limit := range limits {
			field := "project.capacity." + key
			if limit.Kind == yaml.ScalarNode && limit.Tag == "!!null" {
				capacity[key] = nil
				continue
			}
			value, err := text(limit, field, 19)
			if err == nil && patternLimit.MatchString(value) {
				_, err = strconv.ParseInt(value, 10, 64)
			} else {
				err = strconv.ErrSyntax
			}
			if err != nil {
				return failure(field, limit.Line, "expected null or a nonnegative decimal string within int64")
			}
			capacity[key] = value
		}
		p.Config["capacity"] = capacity
	}
	if node := block["git_credential"]; node != nil {
		reference, err := text(node, "project.git_credential", 63)
		if err != nil {
			return err
		}
		if !patternOptionalName.MatchString(reference) {
			return failure("project.git_credential", node.Line, "expected a credential name or an empty string")
		}
		if explicit && repository == "" && reference != "" {
			return failure("project.git_credential", node.Line, "a Git credential reference requires a project repository")
		}
		p.Config["git_credential"] = reference
	}
	return nil
}

func parseImage(node *yaml.Node) (*Image, error) {
	block, err := fields(node, "image", "playbook")
	if err != nil {
		return nil, err
	}
	if block["playbook"] == nil {
		return nil, failure("image.playbook", node.Line, "required field is missing")
	}
	playbook, err := text(block["playbook"], "image.playbook", 1024)
	if err != nil {
		return nil, err
	}
	if reason := playbookFault(playbook); reason != "" {
		return nil, failure("image.playbook", block["playbook"].Line, reason)
	}
	return &Image{Playbook: playbook}, nil
}

// playbookFault называет нарушение правила пути playbook или возвращает "".
func playbookFault(value string) string {
	switch {
	case value == "":
		return "expected a playbook path inside the repository"
	case hasControl(value) || strings.Contains(value, `\`):
		return "control characters and backslashes are forbidden"
	case strings.HasPrefix(value, "/") || patternDrive.MatchString(value):
		return "expected a path relative to the repository root"
	case strings.HasPrefix(value, "-"):
		return "a leading hyphen is forbidden"
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return `".." segments are forbidden`
		}
	}
	if path.Clean(value) != value {
		return "expected a clean path without empty or dot segments"
	}
	if !strings.HasSuffix(value, ".yml") && !strings.HasSuffix(value, ".yaml") {
		return "expected a .yml or .yaml file"
	}
	return ""
}

// Load reads directory/Broodfile. An absent project.repository becomes the
// Git origin of that checkout, normalized to HTTPS; Git itself is optional.
func Load(directory string) (*Project, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if absolute, err = filepath.EvalSymlinks(absolute); err != nil {
		return nil, err
	}
	file, err := os.Open(filepath.Join(absolute, filename))
	if err != nil {
		return nil, fmt.Errorf("could not read %s; it is missing or inaccessible", filename)
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", filename)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDocumentBytes+1))
	if err != nil {
		return nil, fmt.Errorf("could not read %s", filename)
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
		return nil, fmt.Errorf("project.repository: Git origin: %w", err)
	}
	project.Config["repository"] = canonical
	project.RepositoryResolved = true
	return project, nil
}
