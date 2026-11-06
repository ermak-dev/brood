// Строгий YAML-разбор документа: один документ, без якорей, алиасов,
// merge-ключей и повторов ключей; неизвестные поля отвергаются.
package pipeline

import (
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxDocumentBytes = 1024 * 1024
	nestingLimit     = 64
)

var patternLine = regexp.MustCompile(`line (\d+)`)

var allowedTags = map[string]bool{
	"!!null": true, "!!bool": true, "!!int": true,
	"!!float": true, "!!str": true, "!!seq": true, "!!map": true,
}

// decodeDocument разбирает и структурно проверяет YAML. Неизвестные
// поля отвергаются при обходе уже проверенного дерева узлов YAML.
func decodeDocument(text, filename string, limitSize bool) (*Document, error) {
	fail := func(reason string, line int) error {
		if line > 0 {
			return fmt.Errorf("%s: document (line %d): %s", filename, line, reason)
		}
		return fmt.Errorf("%s: document: %s", filename, reason)
	}
	if limitSize && len(text) > maxDocumentBytes {
		return nil, fail("document exceeds 1 MiB", 1)
	}
	var root yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(text))
	if err := decoder.Decode(&root); err != nil {
		if err == io.EOF {
			return nil, fail("empty document", 1)
		}
		return nil, fail("invalid YAML", yamlErrorLine(err))
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		// Второй документ в потоке запрещён.
		return nil, fail("expected a single YAML document", 1)
	}
	if err := checkYAMLNodes(&root, 0); err != nil {
		return nil, fail(err.Error(), rootLine(&root))
	}
	document := &Document{}
	if err := decodeCheckedYAML(root.Content[0], document); err != nil {
		return nil, fail("invalid pipeline document: "+err.Error(), yamlErrorLine(err))
	}
	if document.APIVersion != "brood.dev/v1alpha1" {
		return nil, fail("apiVersion must be brood.dev/v1alpha1", 1)
	}
	return document, nil
}

// decodeCheckedYAML вызывается только после проверки структуры и тегов дерева.
func decodeCheckedYAML(node *yaml.Node, document *Document) error {
	var errors []string
	var walk func(*yaml.Node, reflect.Value) (bool, error)
	walk = func(n *yaml.Node, out reflect.Value) (bool, error) {
		decode := func(node *yaml.Node) (bool, error) {
			err := node.Decode(out.Addr().Interface())
			if err == nil {
				if node.Kind == yaml.ScalarNode && node.ShortTag() == "!!null" {
					return out.Kind() == reflect.Pointer || out.Kind() == reflect.Map || out.Kind() == reflect.Slice || out.Kind() == reflect.Interface, nil
				}
				return true, nil
			}
			if typed, ok := err.(*yaml.TypeError); ok {
				errors = append(errors, typed.Errors...)
				return false, nil
			}
			return false, err
		}
		if n.Kind == yaml.ScalarNode {
			return decode(n)
		}
		if out.Kind() == reflect.Pointer && n.ShortTag() != "!!null" {
			if out.IsNil() {
				out.Set(reflect.New(out.Type().Elem()))
			}
			out = out.Elem()
		}
		switch {
		case n.Kind == yaml.MappingNode && out.Kind() == reflect.Struct:
			fields := make(map[string]int, out.NumField())
			for i := 0; i < out.NumField(); i++ {
				field := out.Type().Field(i)
				name := strings.Split(field.Tag.Get("yaml"), ",")[0]
				if name == "" || name == "-" {
					return false, fmt.Errorf("unsupported YAML struct field %s", field.Name)
				}
				fields[name] = i
			}
			for i := 0; i < len(n.Content); i += 2 {
				key := n.Content[i]
				field, ok := fields[key.Value]
				if !ok {
					errors = append(errors, fmt.Sprintf("line %d: field %s not found in type %s", key.Line, key.Value, out.Type()))
					continue
				}
				if _, err := walk(n.Content[i+1], out.Field(field)); err != nil {
					return false, err
				}
			}
			return true, nil
		case n.Kind == yaml.MappingNode && out.Kind() == reflect.Map && out.Type().Key().Kind() == reflect.String:
			if out.IsNil() {
				out.Set(reflect.MakeMap(out.Type()))
			}
			for i := 0; i < len(n.Content); i += 2 {
				value := reflect.New(out.Type().Elem()).Elem()
				ok, err := walk(n.Content[i+1], value)
				if err != nil {
					return false, err
				}
				if ok || n.Content[i+1].ShortTag() == "!!null" {
					out.SetMapIndex(reflect.ValueOf(n.Content[i].Value).Convert(out.Type().Key()), value)
				}
			}
			return true, nil
		case n.Kind == yaml.SequenceNode && out.Kind() == reflect.Slice:
			out.Set(reflect.MakeSlice(out.Type(), 0, len(n.Content)))
			for _, item := range n.Content {
				value := reflect.New(out.Type().Elem()).Elem()
				ok, err := walk(item, value)
				if err != nil {
					return false, err
				}
				if ok {
					out.Set(reflect.Append(out, value))
				}
			}
			return true, nil
		default:
			// Неверный тип коллекции: пустая копия сохраняет сообщение yaml.v3
			// без повторной квадратичной проверки ключей.
			copy := *n
			copy.Content = nil
			return decode(&copy)
		}
	}
	_, err := walk(node, reflect.ValueOf(document).Elem())
	if err != nil {
		return err
	}
	if len(errors) > 0 {
		return &yaml.TypeError{Errors: errors}
	}
	return nil
}

// rootLine возвращает строку корневого узла для сообщений.
func rootLine(node *yaml.Node) int {
	if node.Line > 0 {
		return node.Line
	}
	if len(node.Content) > 0 {
		return node.Content[0].Line
	}
	return 1
}

// checkYAMLNodes отвергает якоря, алиасы, merge-ключи, повторы ключей,
// нестроковые ключи, неизвестные теги и слишком глубокую вложенность.
func checkYAMLNodes(node *yaml.Node, depth int) error {
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return fmt.Errorf("YAML anchors and aliases are forbidden")
	}
	if depth >= nestingLimit {
		return fmt.Errorf("nesting limit exceeded")
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) == 0 {
			return fmt.Errorf("empty document")
		}
		return checkYAMLNodes(node.Content[0], depth+1)
	case yaml.MappingNode:
		if !allowedTags[node.Tag] {
			return fmt.Errorf("unsupported YAML tag")
		}
		seen := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fmt.Errorf("mapping keys must be strings")
			}
			if key.Value == "<<" {
				return fmt.Errorf("YAML merge keys are forbidden")
			}
			if seen[key.Value] {
				return fmt.Errorf("duplicate mapping key %q", key.Value)
			}
			if key.Anchor != "" {
				return fmt.Errorf("YAML anchors and aliases are forbidden")
			}
			seen[key.Value] = true
			if err := checkYAMLNodes(value, depth+1); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		if !allowedTags[node.Tag] {
			return fmt.Errorf("unsupported YAML tag")
		}
		for _, item := range node.Content {
			if err := checkYAMLNodes(item, depth+1); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if !allowedTags[node.Tag] {
			return fmt.Errorf("unsupported YAML tag")
		}
	}
	return nil
}

// yamlErrorLine достаёт строку из отказа yaml.v3, иначе первая строка.
func yamlErrorLine(err error) int {
	if match := patternLine.FindStringSubmatch(err.Error()); match != nil {
		if line, convErr := strconv.Atoi(match[1]); convErr == nil {
			return line
		}
	}
	return 1
}
