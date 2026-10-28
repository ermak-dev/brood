// Чтение декларации и pins из выбранного Git commit проекта.
// Порт brood/dev/build/project.py (issue #35).
package build

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Project — загруженный проект: корень, commit, грязнота и план.
type Project struct {
	Root        string
	Revision    string
	WorkingTree bool
	Dirty       bool
	Plan        map[string]any
}

// parsePinsJSON читает JSON-объект пинов, отвергая дубликаты ключей и глубину.
func parsePinsJSON(text string) (map[string]string, error) {
	value, err := decodeOneJSON(text)
	if err != nil {
		return nil, fmt.Errorf("pinsFile must contain a JSON object of string pins")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("pinsFile must contain a JSON object of string pins")
	}
	pins := make(map[string]string, len(object))
	for name, item := range object {
		stringValue, ok := item.(string)
		if !ok {
			// Python падает позже, в проверке пинов плана; сообщение то же.
			return nil, failure("Broodfile", "pins", -1,
				"expected nonempty bounded string pin values without controls")
		}
		pins[name] = stringValue
	}
	return pins, nil
}

// decodeOneJSON читает ровно одно JSON-значение, отвергая дубликаты ключей.
func decodeOneJSON(text string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	value, err := parseJSONValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON content")
	}
	return value, nil
}

// parseJSONValue читает одно значение потоком токенов, замечая дубликаты ключей.
func parseJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 500 {
		return nil, fmt.Errorf("JSON nesting limit exceeded")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch typed := token.(type) {
	case json.Delim:
		switch typed {
		case '{':
			object := map[string]any{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, fmt.Errorf("duplicate JSON key check requires string keys")
				}
				if _, exists := object[key]; exists {
					return nil, fmt.Errorf("duplicate JSON key")
				}
				value, err := parseJSONValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				object[key] = value
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return object, nil
		case '[':
			list := []any{}
			for decoder.More() {
				value, err := parseJSONValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				list = append(list, value)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return list, nil
		}
		return nil, fmt.Errorf("unexpected JSON delimiter")
	default:
		return token, nil
	}
}

// readWorkingDocument bounds reads and confines symlinks to the selected root.
func readWorkingDocument(root *os.Root, name string) (string, error) {
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("could not read %s; path is missing, inaccessible, or escapes project root", name)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s must be a regular file", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxDocumentBytes+1))
	if err != nil {
		return "", fmt.Errorf("could not read %s", name)
	}
	if len(data) > maxDocumentBytes {
		return "", fmt.Errorf("%s exceeds 1 MiB", name)
	}
	return string(data), nil
}

// LoadWorkingProject reads path/Broodfile and pins without requiring Git.
// projectName is an optional logical destination label, never a filesystem path.
// brood.local is an unpublished image identity, not a registry destination.
func LoadWorkingProject(path, target, projectName string) (*Project, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	text, err := readWorkingDocument(root, "Broodfile")
	if err != nil {
		return nil, err
	}
	document, err := ParseDocument(text, "Broodfile")
	if err != nil {
		return nil, err
	}
	var pins map[string]string
	if raw, ok := document["pinsFile"]; ok && raw != nil {
		nameValue, _ := raw.(string)
		name, err := SafePath(nameValue, "pinsFile")
		if err != nil {
			return nil, err
		}
		text, err := readWorkingDocument(root, name)
		if err != nil {
			return nil, err
		}
		pins, err = parsePinsJSON(text)
		if err != nil {
			return nil, err
		}
	}
	if projectName == "" {
		projectName, _ = document["name"].(string)
	}
	plan, err := MakePlan(document, pins, target, "brood.local", projectName, "Broodfile")
	if err != nil {
		return nil, err
	}
	return &Project{Root: absolute, WorkingTree: true, Plan: plan}, nil
}

// LoadProject не читает рабочие Broodfile и pins, даже если они изменены.
func LoadProject(path, ref, target, registry, atespace string) (*Project, error) {
	if ref == "" {
		ref = "HEAD"
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	toplevel, err := Git(absolute, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(toplevel)
	sha, err := Revision(root, ref)
	if err != nil {
		return nil, err
	}
	text, err := Git(root, "show", sha+":Broodfile")
	if err != nil {
		return nil, err
	}
	document, err := ParseDocument(text, "Broodfile")
	if err != nil {
		return nil, err
	}
	var pins map[string]string
	if raw, ok := document["pinsFile"]; ok && raw != nil {
		nameValue, _ := raw.(string)
		name, err := SafePath(nameValue, "pinsFile")
		if err != nil {
			return nil, err
		}
		pinsText, err := Git(root, "show", sha+":"+name)
		if err != nil {
			return nil, err
		}
		if len(pinsText) > maxDocumentBytes {
			return nil, fmt.Errorf("pinsFile exceeds 1 MiB")
		}
		pins, err = parsePinsJSON(pinsText)
		if err != nil {
			return nil, err
		}
	}
	resolved, err := MakePlan(document, pins, target, registry, atespace, "Broodfile")
	if err != nil {
		return nil, err
	}
	planSources, _ := resolved["sources"].(map[string]any)
	for _, name := range sortedKeys(planSources) {
		source, _ := planSources[name].(map[string]any)
		if source["type"] != "project" {
			continue
		}
		source["revision"] = sha
		gitMetadata, _ := source["gitMetadata"].(bool)
		remote, present, err := Origin(root, gitMetadata)
		if err != nil {
			return nil, err
		}
		if present {
			source["repository"] = remote
		}
	}
	status, err := Git(root, "status", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	return &Project{Root: root, Revision: sha, Dirty: status != "", Plan: resolved}, nil
}
