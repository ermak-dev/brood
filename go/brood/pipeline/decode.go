// Строгий YAML-разбор документа: один документ, без якорей, алиасов,
// merge-ключей и повторов ключей; неизвестные поля отвергаются.
package pipeline

import (
	"fmt"
	"io"
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
// поля отвергаются: KnownFields(true) на втором проходе по тому же
// тексту, после проверки дерева на якоря и дубликаты ключей.
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
	strict := yaml.NewDecoder(strings.NewReader(text))
	// Опечатка в имени поля не должна молча терять значение.
	strict.KnownFields(true)
	document := &Document{}
	if err := strict.Decode(document); err != nil {
		return nil, fail("invalid pipeline document: "+err.Error(), yamlErrorLine(err))
	}
	if document.APIVersion != "brood.dev/v1alpha1" {
		return nil, fail("apiVersion must be brood.dev/v1alpha1", 1)
	}
	return document, nil
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
