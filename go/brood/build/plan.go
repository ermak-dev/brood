// Чистый разбор Broodfile и разрешение плана без процессов и сети.
// Порт brood/dev/build/plan.py (issue #35): та же строгость, те же сообщения.
package build

import (
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"brood.dev/go/brood/gitorigin"
	"brood.dev/go/brood/pipeline"
)

const (
	maxDocumentBytes = 1024 * 1024
	nestingLimit     = 64
)

var (
	patternName     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	patternPin      = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	patternHex40    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	patternDecimal  = regexp.MustCompile(`^[1-9][0-9]*$`)
	patternEnvName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	patternRepoPart = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	patternDnsLabel = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
	patternGitUser  = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	patternScp      = regexp.MustCompile(`^git@([^:/]+):(.+)$`)
	patternAtespace = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	patternField    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	patternLine     = regexp.MustCompile(`line (\d+)`)
	patternDrive    = regexp.MustCompile(`^[A-Za-z]:`)
)

// patternOptionalName — имя или пустая строка, явно снимающая ссылку.
var patternOptionalName = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]*[a-z0-9])?)?$`)

// failure строит отказ «файл: поле (строка): причина» без трассировки.
func failure(filename, field string, line int, reason string) error {
	location := filename + ": " + field
	if line > 0 {
		location += fmt.Sprintf(" (line %d)", line)
	}
	return fmt.Errorf("%s: %s", location, reason)
}

// fieldName собирает имя поля из пути, скрывая нестандартные части.
func fieldName(path []any) string {
	if len(path) == 0 {
		return "document"
	}
	parts := make([]string, len(path))
	for i, part := range path {
		text := fmt.Sprint(part)
		if patternField.MatchString(text) {
			parts[i] = text
		} else {
			parts[i] = "[field]"
		}
	}
	return strings.Join(parts, ".")
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

// hasWhitespace ищет любой пробельный символ Unicode.
func hasWhitespace(value string) bool {
	for _, char := range value {
		if unicode.IsSpace(char) {
			return true
		}
	}
	return false
}

var allowedTags = map[string]bool{
	"!!null": true, "!!bool": true, "!!int": true,
	"!!float": true, "!!str": true, "!!seq": true, "!!map": true,
}

// pathKey кодирует путь для карты номеров строк.
func pathKey(path []any) string {
	parts := make([]string, len(path))
	for i, part := range path {
		parts[i] = fmt.Sprint(part)
	}
	return strings.Join(parts, "\x00")
}

// checkNodes проверяет дерево YAML: теги, ключи, дубликаты, глубину; собирает строки.
func checkNodes(node *yaml.Node, filename string, locations map[string]int, path []any, depth int) error {
	locations[pathKey(path)] = node.Line
	if node.Kind == yaml.AliasNode {
		return failure(filename, "document", node.Line, "YAML anchors and aliases are forbidden")
	}
	if node.Anchor != "" {
		return failure(filename, "document", node.Line, "YAML anchors and aliases are forbidden")
	}
	if depth >= nestingLimit {
		return failure(filename, "document", node.Line, "nesting limit exceeded")
	}
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) == 0 {
			return failure(filename, "document", 1, "empty document")
		}
		return checkNodes(node.Content[0], filename, locations, path, depth+1)
	case yaml.MappingNode:
		if !allowedTags[node.Tag] {
			return failure(filename, fieldName(path), node.Line, "unsupported YAML tag")
		}
		seen := map[string]bool{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return failure(filename, fieldName(path), key.Line, "mapping keys must be strings")
			}
			if key.Value == "<<" {
				return failure(filename, fieldName(path), key.Line, "YAML merge keys are forbidden")
			}
			if seen[key.Value] {
				return failure(filename, fieldName(path), key.Line, "duplicate mapping key")
			}
			seen[key.Value] = true
			if err := checkNodes(value, filename, locations, append(path, key.Value), depth+1); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		if !allowedTags[node.Tag] {
			return failure(filename, fieldName(path), node.Line, "unsupported YAML tag")
		}
		for index, item := range node.Content {
			if err := checkNodes(item, filename, locations, append(path, index), depth+1); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if !allowedTags[node.Tag] {
			return failure(filename, fieldName(path), node.Line, "unsupported YAML tag")
		}
	}
	return nil
}

// nodeValue превращает проверенное дерево в значения с json-подобной типизацией.
func nodeValue(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) == 0 {
			return nil, nil
		}
		return nodeValue(node.Content[0])
	case yaml.MappingNode:
		result := map[string]any{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			value, err := nodeValue(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			result[node.Content[i].Value] = value
		}
		return result, nil
	case yaml.SequenceNode:
		result := make([]any, 0, len(node.Content))
		for _, item := range node.Content {
			value, err := nodeValue(item)
			if err != nil {
				return nil, err
			}
			result = append(result, value)
		}
		return result, nil
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!null":
			return nil, nil
		case "!!bool":
			return node.Value == "true", nil
		case "!!int":
			// Переполнение int64 не отвергаем: тип значения остаётся числом,
			// а диапазон uint64 проверяет план по строковому значению.
			if number, err := strconv.ParseInt(node.Value, 0, 64); err == nil {
				return number, nil
			}
			if number, err := strconv.ParseUint(node.Value, 0, 64); err == nil {
				return number, nil
			}
			if number, err := strconv.ParseFloat(node.Value, 64); err == nil {
				return number, nil
			}
			return nil, fmt.Errorf("invalid YAML structure")
		case "!!float":
			number, err := strconv.ParseFloat(node.Value, 64)
			if err != nil {
				return nil, fmt.Errorf("invalid YAML structure")
			}
			return number, nil
		default:
			return node.Value, nil
		}
	}
	return nil, fmt.Errorf("invalid YAML structure")
}

// yamlErrorLine достаёт строку из отказа yaml.v3, иначе первая строка.
func yamlErrorLine(err error) int {
	if match := patternLine.FindStringSubmatch(err.Error()); match != nil {
		line, convErr := strconv.Atoi(match[1])
		if convErr == nil {
			return line
		}
	}
	return 1
}

// ParseDocument возвращает независимый документ после безопасного YAML-разбора и схемы.
func ParseDocument(text, filename string) (map[string]any, error) {
	if len(text) > maxDocumentBytes {
		return nil, failure(filename, "document", 1, "document exceeds 1 MiB")
	}
	decoder := yaml.NewDecoder(strings.NewReader(text))
	var root yaml.Node
	if err := decoder.Decode(&root); err != nil {
		if err == io.EOF {
			return nil, failure(filename, "document", 1, "empty document")
		}
		return nil, failure(filename, "document", yamlErrorLine(err), "invalid YAML")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		// Второй документ в потоке запрещён, как get_single_node в Python.
		return nil, failure(filename, "document", 1, "invalid YAML")
	}
	locations := map[string]int{}
	if err := checkNodes(&root, filename, locations, nil, 0); err != nil {
		return nil, err
	}
	document, err := nodeValue(&root)
	if err != nil {
		return nil, failure(filename, "document", 1, "invalid YAML structure")
	}
	if err := validateSchema(document, filename, locations); err != nil {
		return nil, err
	}
	typed, ok := document.(map[string]any)
	if !ok {
		return nil, failure(filename, "document", 1, "invalid document structure")
	}
	if err := validateWorkflows(typed, filename); err != nil {
		return nil, err
	}
	return typed, nil
}

// Схема Broodfile: точный перенос schema.json c сохранением порядка свойств.
type schemaNode struct {
	typ         string
	enum        []any
	constV      any
	constSet    bool
	minLen      int
	maxLen      int
	pattern     *regexp.Regexp
	minP        *int64
	maxP        *int64
	minItems    int
	maxItems    int
	uniqueItems bool
	items       *schemaNode
	minProps    int
	maxProps    *int64
	propNames   *schemaNode
	required    []string
	addlFalse   bool
	addlProps   *schemaNode
	props       []schemaProperty
	ref         *schemaNode
	allOf       []*schemaNode
	anyOf       []*schemaNode
	oneOf       []*schemaNode
	notS        *schemaNode
	ifS         *schemaNode
	thenS       *schemaNode
}

type schemaProperty struct {
	name   string
	schema *schemaNode
}

func schemaProperties(pairs ...any) []schemaProperty {
	result := make([]schemaProperty, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		result = append(result, schemaProperty{pairs[i].(string), pairs[i+1].(*schemaNode)})
	}
	return result
}

func numberPtr(value int64) *int64 { return &value }

var schemaRoot *schemaNode

// init строит схему один раз, связывая локальные ссылки $defs.
func init() {
	name := &schemaNode{typ: "string", maxLen: 63, pattern: patternName}
	path := &schemaNode{typ: "string", minLen: 1, maxLen: 1024}
	pinName := &schemaNode{typ: "string", pattern: patternPin}
	strings := &schemaNode{typ: "array", maxItems: 256, items: &schemaNode{typ: "string", maxLen: 16384}}
	boolean := &schemaNode{typ: "boolean"}

	capability := &schemaNode{enum: []any{
		"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID", "KILL", "SETGID", "SETUID",
		"SETPCAP", "LINUX_IMMUTABLE", "NET_BIND_SERVICE", "NET_BROADCAST", "NET_ADMIN", "NET_RAW",
		"IPC_LOCK", "IPC_OWNER", "SYS_MODULE", "SYS_RAWIO", "SYS_CHROOT", "SYS_PTRACE", "SYS_PACCT",
		"SYS_ADMIN", "SYS_BOOT", "SYS_NICE", "SYS_RESOURCE", "SYS_TIME", "SYS_TTY_CONFIG", "MKNOD",
		"LEASE", "AUDIT_WRITE", "AUDIT_CONTROL", "SETFCAP", "MAC_OVERRIDE", "MAC_ADMIN", "SYSLOG",
		"WAKE_ALARM", "BLOCK_SUSPEND", "AUDIT_READ", "PERFMON", "BPF", "CHECKPOINT_RESTORE",
	}}
	ports := &schemaNode{typ: "integer", minP: numberPtr(1), maxP: numberPtr(65535)}
	rule := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"hosts", "ports"},
		props: schemaProperties(
			"hosts", &schemaNode{typ: "array", maxItems: 256, uniqueItems: true,
				items: &schemaNode{typ: "string", maxLen: 253}},
			"ports", &schemaNode{typ: "array", minItems: 1, maxItems: 64, uniqueItems: true, items: ports},
		),
	}
	mount := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"name", "mountPath"},
		props: schemaProperties(
			"name", name,
			"mountPath", &schemaNode{typ: "string", pattern: regexp.MustCompile(`^/`), maxLen: 1024},
		),
	}
	envItem := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"name", "value"},
		props: schemaProperties(
			"name", &schemaNode{typ: "string", pattern: patternEnvName},
			"value", &schemaNode{typ: "string", maxLen: 16384},
		),
	}
	container := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"name", "command"},
		props: schemaProperties(
			"name", name,
			"command", &schemaNode{ref: strings, minItems: 1},
			"args", strings,
			"env", &schemaNode{typ: "array", maxItems: 128, items: envItem},
			"volumeMounts", &schemaNode{typ: "array", maxItems: 32, items: mount},
			"securityContext", &schemaNode{
				typ: "object", addlFalse: true, required: []string{"capabilities"},
				props: schemaProperties("capabilities", &schemaNode{
					typ: "object", addlFalse: true,
					props: schemaProperties(
						"add", &schemaNode{typ: "array", uniqueItems: true, items: capability},
						"drop", &schemaNode{typ: "array", uniqueItems: true, items: capability},
					),
				}),
			},
		),
	}
	volume := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"name", "durableDir"},
		props: schemaProperties(
			"name", name,
			"durableDir", &schemaNode{typ: "object", maxProps: numberPtr(0)},
		),
	}
	limits := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"cpuMillicores", "memoryBytes"},
		props: schemaProperties(
			"cpuMillicores", &schemaNode{typ: "string", maxLen: 20, pattern: patternDecimal},
			"memoryBytes", &schemaNode{typ: "string", maxLen: 20, pattern: patternDecimal},
		),
	}
	actor := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"resources", "containers"},
		props: schemaProperties(
			"workerSelector", &schemaNode{
				typ: "object", addlFalse: true,
				props: schemaProperties("matchLabels", &schemaNode{
					typ: "object", addlProps: &schemaNode{typ: "string"},
				}),
			},
			"resources", &schemaNode{
				typ: "object", addlFalse: true, required: []string{"limits"},
				props: schemaProperties("limits", limits),
			},
			"containers", &schemaNode{typ: "array", minItems: 1, maxItems: 1, items: container},
			"volumes", &schemaNode{typ: "array", maxItems: 32, items: volume},
			"snapshotConfig", &schemaNode{
				typ: "object", addlFalse: true,
				props: schemaProperties(
					"preferredFidelity", &schemaNode{typ: "string", enum: []any{"SNAPSHOT_FIDELITY_VOLUMES", "SNAPSHOT_FIDELITY_MEMORY"}},
					"goldenSnapshotConfig", &schemaNode{
						typ: "object", addlFalse: true, required: []string{"mode"},
						props: schemaProperties("mode", &schemaNode{
							typ: "string", constV: "GOLDEN_SNAPSHOT_MODE_DISABLED", constSet: true,
						}),
					},
				),
			},
		),
	}
	network := &schemaNode{
		typ: "object", addlFalse: true,
		props: schemaProperties("tls", rule, "http", rule),
	}
	source := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"type", "destination"},
		props: schemaProperties(
			"type", &schemaNode{enum: []any{"project", "git"}},
			"destination", path,
			"gitMetadata", boolean,
			"repository", &schemaNode{typ: "string", minLen: 1, maxLen: 4096},
			"revision", &schemaNode{typ: "string", pattern: patternHex40},
			"revisionPin", pinName,
		),
		allOf: []*schemaNode{
			{
				ifS: &schemaNode{props: schemaProperties("type", &schemaNode{
					typ: "string", constV: "git", constSet: true,
				})},
				thenS: &schemaNode{
					required: []string{"repository"},
					oneOf: []*schemaNode{
						{required: []string{"revision"}, notS: &schemaNode{required: []string{"revisionPin"}}},
						{required: []string{"revisionPin"}, notS: &schemaNode{required: []string{"revision"}}},
					},
				},
			},
			{
				ifS: &schemaNode{props: schemaProperties("type", &schemaNode{
					typ: "string", constV: "project", constSet: true,
				})},
				thenS: &schemaNode{notS: &schemaNode{anyOf: []*schemaNode{
					{required: []string{"repository"}},
					{required: []string{"revision"}},
					{required: []string{"revisionPin"}},
				}}},
			},
		},
	}
	sources := &schemaNode{
		typ: "object", minProps: 1, propNames: name, addlProps: source,
	}
	build := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"dockerfile", "platform"},
		props: schemaProperties(
			"dockerfile", path,
			"platform", &schemaNode{enum: []any{"linux/amd64", "linux/arm64"}},
			"arguments", &schemaNode{
				typ: "object", propNames: pinName,
				addlProps: &schemaNode{typ: "string", maxLen: 16384},
			},
			"argsFromPins", &schemaNode{typ: "array", uniqueItems: true, items: pinName},
		),
	}
	environment := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"sources", "build"},
		props: schemaProperties(
			"sources", &schemaNode{typ: "array", minItems: 1, uniqueItems: true, items: name},
			"build", build,
		),
	}
	template := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"environment", "network", "actor"},
		props: schemaProperties("environment", name, "network", name, "actor", actor),
	}
	targetEntry := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"template"},
		props: schemaProperties("template", name),
	}
	text := &schemaNode{typ: "string"}
	option := &schemaNode{typ: "object", addlFalse: true, required: []string{"label", "outcome"}, props: schemaProperties("label", text, "outcome", text)}
	// Роль узла workflow: DNS label, обязательна у actor-узлов (#208).
	// Схема проверяет форму и привязку к типу узла; обязательность для
	// actor-узлов проверяет общий валидатор конвейера ниже по потоку.
	childJoin := &schemaNode{typ: "object", addlFalse: true, props: schemaProperties(
		"mode", &schemaNode{typ: "string", enum: []any{"all", "any", "quorum"}},
		"quorum", &schemaNode{typ: "integer", minP: numberPtr(0), maxP: numberPtr(64)}),
		allOf: []*schemaNode{
			{ifS: &schemaNode{required: []string{"mode"}, props: schemaProperties("mode", &schemaNode{constV: "quorum", constSet: true})},
				thenS: &schemaNode{required: []string{"quorum"}, props: schemaProperties("quorum", &schemaNode{minP: numberPtr(1)})}},
			{ifS: &schemaNode{required: []string{"quorum"}, props: schemaProperties("quorum", &schemaNode{minP: numberPtr(1)})},
				thenS: &schemaNode{required: []string{"mode"}, props: schemaProperties("mode", &schemaNode{constV: "quorum", constSet: true})}},
		}}
	node := &schemaNode{typ: "object", addlFalse: true, required: []string{"type"}, props: schemaProperties(
		"type", &schemaNode{typ: "string", enum: []any{"actor", "human", "condition", "land", "exit", "parallel", "fan_in"}}, "role", name, "prompt", text, "question", text,
		"options", &schemaNode{typ: "array", items: option}, "target", text, "branches", &schemaNode{typ: "array", items: text}, "childJoin", childJoin),
		allOf: []*schemaNode{
			{ifS: &schemaNode{required: []string{"role"}}, thenS: &schemaNode{props: schemaProperties("type", &schemaNode{constV: "actor", constSet: true})}},
			{ifS: &schemaNode{required: []string{"childJoin"}}, thenS: &schemaNode{props: schemaProperties("type", &schemaNode{constV: "actor", constSet: true})}},
		}}
	edge := &schemaNode{typ: "object", addlFalse: true, required: []string{"from", "to"}, props: schemaProperties("from", text, "to", text, "condition", text, "label", text, "weight", &schemaNode{typ: "integer"})}
	workflow := &schemaNode{typ: "object", addlFalse: true, required: []string{"start", "exit", "nodes", "edges", "goalGates"}, props: schemaProperties(
		"start", text, "exit", text, "nodes", &schemaNode{typ: "object", minProps: 1, propNames: name, addlProps: node},
		"edges", &schemaNode{typ: "array", items: edge}, "goalGates", &schemaNode{typ: "array", items: text},
		"retry", &schemaNode{typ: "object", addlFalse: true, required: []string{"maxRetries"}, props: schemaProperties("maxRetries", &schemaNode{typ: "integer", minP: numberPtr(0)})})}
	capacityLimit := &schemaNode{anyOf: []*schemaNode{
		{typ: "null"},
		{typ: "string", maxLen: 19, pattern: regexp.MustCompile(`^(0|[1-9][0-9]*)$`)},
	}}
	projectCapacity := &schemaNode{
		typ: "object", addlFalse: true, required: []string{"worker_selector"},
		props: schemaProperties(
			"worker_selector", &schemaNode{typ: "object", minProps: 1, addlProps: &schemaNode{typ: "string"}},
			"max_swarms", capacityLimit, "max_actors", capacityLimit,
			"max_cpu_millicores", capacityLimit, "max_memory_bytes", capacityLimit,
		),
	}
	projectConfig := &schemaNode{typ: "object", addlFalse: true, props: schemaProperties(
		"repository", &schemaNode{typ: "string", maxLen: gitorigin.MaxLength},
		"description", &schemaNode{typ: "string", maxLen: 4096},
		"capacity", projectCapacity,
		// Ссылка на серверный Git credential проекта (#173): имя, не значение.
		"git_credential", &schemaNode{typ: "string", maxLen: 63, pattern: patternOptionalName},
	)}
	apiVersion := &schemaNode{typ: "string", constV: "brood.dev/v1alpha1", constSet: true}
	schemaRoot = &schemaNode{
		typ: "object", addlFalse: true,
		required: []string{"apiVersion", "name", "defaultTarget", "sources",
			"environments", "templates", "networks", "targets"},
		props: schemaProperties(
			"apiVersion", apiVersion,
			"name", name,
			"project", projectConfig,
			"defaultTarget", name,
			"pinsFile", path,
			"sources", sources,
			"environments", &schemaNode{typ: "object", minProps: 1, propNames: name, addlProps: environment},
			"templates", &schemaNode{typ: "object", minProps: 1, propNames: name, addlProps: template},
			"networks", &schemaNode{typ: "object", minProps: 1, propNames: name, addlProps: network},
			"targets", &schemaNode{typ: "object", minProps: 1, propNames: name, addlProps: targetEntry},
			"workflows", &schemaNode{typ: "object", propNames: name, addlProps: workflow},
		),
	}
}

type schemaError struct {
	path    []any
	keyword string
}

// schemaTypeMatches проверяет json-тип значения.
func schemaTypeMatches(typ string, value any) bool {
	switch typ {
	case "null":
		return value == nil
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		case float64:
			number := value.(float64)
			return !math.IsInf(number, 0) && number == math.Trunc(number)
		}
		return false
	}
	return false
}

// valuesEqual сравнивает значения схемы без учёта представления чисел.
func valuesEqual(left, right any) bool {
	if reflect.DeepEqual(left, right) {
		return true
	}
	return canonicalJSON(left) == canonicalJSON(right)
}

// canonicalJSON кодирует значение для сравнения уникальности.
func canonicalJSON(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		keys := sortedKeys(typed)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, strconv.Quote(key)+":"+canonicalJSON(typed[key]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			parts = append(parts, canonicalJSON(item))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case string:
		return strconv.Quote(typed)
	default:
		return fmt.Sprintf("%v", value)
	}
}

func sortedKeys[V any](mapping map[string]V) []string {
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// evalSchema применяет схему и возвращает первый отказ в порядке обхода.
func evalSchema(schema *schemaNode, value any, path []any) *schemaError {
	if schema.typ != "" && !schemaTypeMatches(schema.typ, value) {
		return &schemaError{path, "type"}
	}
	if schema.constSet && !valuesEqual(value, schema.constV) {
		return &schemaError{path, "const"}
	}
	if len(schema.enum) > 0 {
		found := false
		for _, item := range schema.enum {
			if valuesEqual(value, item) {
				found = true
				break
			}
		}
		if !found {
			return &schemaError{path, "enum"}
		}
	}
	if text, ok := value.(string); ok {
		if schema.minLen > 0 && utf8.RuneCountInString(text) < schema.minLen {
			return &schemaError{path, "minLength"}
		}
		if schema.maxLen > 0 && utf8.RuneCountInString(text) > schema.maxLen {
			return &schemaError{path, "maxLength"}
		}
		// Шаблоны схемы заканчиваются lookahead (?![\s\S]): в Go якоря ^...$
		// привязаны к концу текста и дают то же отрицание хвостовых символов.
		if schema.pattern != nil && !schema.pattern.MatchString(text) {
			return &schemaError{path, "pattern"}
		}
	}
	if schema.minP != nil || schema.maxP != nil {
		if number, ok := numericValue(value); ok {
			if schema.minP != nil && number < float64(*schema.minP) {
				return &schemaError{path, "minimum"}
			}
			if schema.maxP != nil && number > float64(*schema.maxP) {
				return &schemaError{path, "maximum"}
			}
		}
	}
	if items, ok := value.([]any); ok {
		if schema.minItems > 0 && len(items) < schema.minItems {
			return &schemaError{path, "minItems"}
		}
		if schema.maxItems > 0 && len(items) > schema.maxItems {
			return &schemaError{path, "maxItems"}
		}
		if schema.uniqueItems {
			seen := map[string]bool{}
			for _, item := range items {
				key := canonicalJSON(item)
				if seen[key] {
					return &schemaError{path, "uniqueItems"}
				}
				seen[key] = true
			}
		}
		if schema.items != nil {
			for index, item := range items {
				if err := evalSchema(schema.items, item, append(path, index)); err != nil {
					return err
				}
			}
		}
	}
	if mapping, ok := value.(map[string]any); ok {
		if err := evalObjectKeywords(schema, mapping, path); err != nil {
			return err
		}
	}
	if schema.ref != nil {
		if err := evalSchema(schema.ref, value, path); err != nil {
			return err
		}
	}
	for _, sub := range schema.allOf {
		if err := evalSchema(sub, value, path); err != nil {
			return err
		}
	}
	if len(schema.anyOf) > 0 {
		matched := false
		for _, sub := range schema.anyOf {
			if evalSchema(sub, value, path) == nil {
				matched = true
				break
			}
		}
		if !matched {
			return &schemaError{path, "anyOf"}
		}
	}
	if len(schema.oneOf) > 0 {
		count := 0
		for _, sub := range schema.oneOf {
			if evalSchema(sub, value, path) == nil {
				count++
			}
		}
		if count != 1 {
			return &schemaError{path, "oneOf"}
		}
	}
	if schema.notS != nil && evalSchema(schema.notS, value, path) == nil {
		return &schemaError{path, "not"}
	}
	if schema.ifS != nil {
		if evalSchema(schema.ifS, value, path) == nil && schema.thenS != nil {
			if err := evalSchema(schema.thenS, value, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// evalObjectKeywords применяет объектные ключевые слова схемы к словарю.
func evalObjectKeywords(schema *schemaNode, mapping map[string]any, path []any) *schemaError {
	if schema.minProps > 0 && len(mapping) < schema.minProps {
		return &schemaError{path, "minProperties"}
	}
	if schema.maxProps != nil && int64(len(mapping)) > *schema.maxProps {
		return &schemaError{path, "maxProperties"}
	}
	if schema.propNames != nil {
		for _, key := range sortedKeys(mapping) {
			if err := evalSchema(schema.propNames, key, path); err != nil {
				// propertyNames оставляет путь у самого объекта.
				return &schemaError{path, err.keyword}
			}
		}
	}
	if schema.addlFalse || schema.addlProps != nil {
		known := map[string]bool{}
		for _, property := range schema.props {
			known[property.name] = true
		}
		for _, key := range sortedKeys(mapping) {
			if known[key] {
				continue
			}
			if schema.addlFalse {
				return &schemaError{path, "additionalProperties"}
			}
			if err := evalSchema(schema.addlProps, mapping[key], append(path, key)); err != nil {
				return err
			}
		}
	}
	for _, name := range schema.required {
		if _, ok := mapping[name]; !ok {
			return &schemaError{path, "required"}
		}
	}
	for _, property := range schema.props {
		value, ok := mapping[property.name]
		if !ok {
			continue
		}
		if err := evalSchema(property.schema, value, append(path, property.name)); err != nil {
			return err
		}
	}
	return nil
}

// numericValue приводит числовое значение к float64 для сравнения границ.
func numericValue(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint64:
		return float64(number), true
	case float64:
		return number, true
	}
	return 0, false
}

// validateSchema применяет схему с поиском номера строки по близкому узлу.
func validateSchema(document any, filename string, locations map[string]int) error {
	if err := evalSchema(schemaRoot, document, nil); err != nil {
		line := -1
		if locations != nil {
			lookup := err.path
			for {
				if found, ok := locations[pathKey(lookup)]; ok {
					line = found
					break
				}
				if len(lookup) == 0 {
					break
				}
				lookup = lookup[:len(lookup)-1]
			}
		}
		return failure(filename, fieldName(err.path), line,
			"schema constraint failed ("+err.keyword+")")
	}
	return nil
}

// cleanPosix нормализует POSIX-путь как PurePosixPath, сохраняя корень.
func cleanPosix(value string) string {
	absolute := strings.HasPrefix(value, "/")
	segments := []string{}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." {
			continue
		}
		segments = append(segments, segment)
	}
	result := strings.Join(segments, "/")
	if absolute {
		result = "/" + result
	}
	if result == "" {
		return "."
	}
	return result
}

// hasDotDotSegment ищет сегмент «..» в пути.
func hasDotDotSegment(value string) bool {
	for _, segment := range strings.Split(value, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// SafePath нормализует относительный POSIX-путь без выхода из контекста.
func SafePath(value, field string) (string, error) {
	if value == "" || hasControl(value) || strings.Contains(value, "\\") ||
		strings.HasPrefix(value, "/") || patternDrive.MatchString(value) ||
		hasDotDotSegment(value) {
		return "", fmt.Errorf("%s: expected a safe relative POSIX path", field)
	}
	result := cleanPosix(value)
	if result == "." {
		return "", fmt.Errorf("%s: empty or dot path is forbidden", field)
	}
	return result, nil
}

// dnsName проверяет точное DNS-имя длиной до 253 символов.
func dnsName(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if !patternDnsLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// urlHost принимает IP или DNS-имя, как _url_host в Python.
func urlHost(value string) bool {
	if value == "" {
		return false
	}
	if net.ParseIP(value) != nil {
		return true
	}
	return dnsName(value)
}

// PublicHostname проверяет точное публичное DNS-имя без локальных суффиксов.
func PublicHostname(value string) (string, error) {
	if !dnsName(value) || !strings.Contains(value, ".") {
		return "", fmt.Errorf("hostname: expected an exact public DNS name")
	}
	host := strings.ToLower(value)
	private := map[string]bool{
		"localhost": true, "local": true, "localdomain": true, "internal": true,
		"intranet": true, "lan": true, "home": true, "corp": true, "private": true,
		"test": true, "invalid": true, "example": true, "onion": true, "arpa": true,
		"svc": true, "cluster": true,
	}
	suffix := host[strings.LastIndex(host, ".")+1:]
	if private[suffix] || allDigits(suffix) {
		return "", fmt.Errorf("hostname: private or special DNS suffix is forbidden")
	}
	if net.ParseIP(host) != nil {
		return "", fmt.Errorf("hostname: IP addresses are forbidden")
	}
	return host, nil
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// Repository принимает только Git URL без credentials, query и fragment.
func Repository(value string) (string, error) {
	message := "repository: expected a credential-free HTTPS, SSH or git@host:path URL"
	if value == "" || hasControl(value) || hasWhitespace(value) || strings.Contains(value, "\\") {
		return "", fmt.Errorf("%s", message)
	}
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "ssh://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", fmt.Errorf("%s", message)
		}
		if parsed.User != nil {
			password, hasPassword := parsed.User.Password()
			if hasPassword || password != "" {
				return "", fmt.Errorf("%s", message)
			}
			if parsed.Scheme == "https" || !patternGitUser.MatchString(parsed.User.Username()) {
				return "", fmt.Errorf("%s", message)
			}
		}
		port := parsed.Port()
		portNumber := 0
		if port != "" {
			portNumber, err = strconv.Atoi(port)
			if err != nil || portNumber < 1 || portNumber > 65535 {
				return "", fmt.Errorf("%s", message)
			}
		}
		if !urlHost(strings.ToLower(parsed.Hostname())) || parsed.RawQuery != "" ||
			parsed.Fragment != "" || strings.ContainsAny(value, "?#") ||
			parsed.Path == "" || parsed.Path == "/" {
			return "", fmt.Errorf("%s", message)
		}
		if _, err := SafePath(strings.TrimLeft(parsed.Path, "/"), "path"); err != nil {
			return "", fmt.Errorf("%s", message)
		}
		return value, nil
	}
	match := patternScp.FindStringSubmatch(value)
	if match == nil || !urlHost(strings.ToLower(match[1])) || strings.ContainsAny(value, "?#") {
		return "", fmt.Errorf("%s", message)
	}
	if _, err := url.PathUnescape(match[2]); err != nil {
		return "", fmt.Errorf("%s", message)
	}
	decoded, _ := url.PathUnescape(match[2])
	if _, err := SafePath(decoded, "path"); err != nil {
		return "", fmt.Errorf("%s", message)
	}
	return value, nil
}

// registryPrefix проверяет префикс OCI-registry без credentials.
func registryPrefix(value string) (string, error) {
	message := "registry: expected a credential-free OCI registry prefix"
	if value == "" || hasControl(value) || hasWhitespace(value) || strings.ContainsAny(value, "@?#\\") {
		return "", fmt.Errorf("%s", message)
	}
	parts := strings.Split(value, "/")
	endpoint, err := url.Parse("//" + parts[0])
	if err != nil || endpoint.User != nil || parts[0] == "" || strings.HasSuffix(parts[0], ":") {
		return "", fmt.Errorf("%s", message)
	}
	port := endpoint.Port()
	if port != "" {
		portNumber, convErr := strconv.Atoi(port)
		if convErr != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("%s", message)
		}
	}
	if !urlHost(strings.ToLower(endpoint.Hostname())) {
		return "", fmt.Errorf("%s", message)
	}
	for _, part := range parts[1:] {
		if !patternRepoPart.MatchString(part) {
			return "", fmt.Errorf("%s", message)
		}
	}
	return value, nil
}

// deepCopyValue снимает независимую копию значения плана.
func deepCopyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = deepCopyValue(item)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, deepCopyValue(item))
		}
		return result
	default:
		return value
	}
}

func asMap(value any) (map[string]any, bool) {
	mapping, ok := value.(map[string]any)
	return mapping, ok
}

func asString(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok
}

// asList приводит значение к срезу, отдавая пустой срез вместо nil.
func asList(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return nil
}

// checked применяет валидатор строки и превращает отказ в отказ плана.
func checked(filename, field, value string, validate func(string) (string, error)) (string, error) {
	result, err := validate(value)
	if err != nil {
		return "", failure(filename, field, -1, err.Error())
	}
	return result, nil
}

// pinValues проверяет словарь пинов и возвращает его копию.
func pinValues(pins map[string]string, filename string) (map[string]string, error) {
	if pins == nil {
		return map[string]string{}, nil
	}
	result := make(map[string]string, len(pins))
	for name, value := range pins {
		if !patternPin.MatchString(name) {
			return nil, failure(filename, "pins", -1, "invalid pin name")
		}
		if value == "" || utf8.RuneCountInString(value) > 16384 || hasControl(value) {
			return nil, failure(filename, "pins", -1,
				"expected nonempty bounded string pin values without controls")
		}
		result[name] = value
	}
	return result, nil
}

// resolvePin достаёт значение пина или отказывает с причиной.
func resolvePin(name string, pins map[string]string, pinsFile any, filename, field string) (string, error) {
	if pinsFile == nil {
		return "", failure(filename, field, -1, "pin reference requires pinsFile")
	}
	value, ok := pins[name]
	if !ok {
		return "", failure(filename, field, -1, "referenced pin is missing")
	}
	return value, nil
}

// resolveReference требует наличия имени в словаре определений.
func resolveReference(name string, definitions map[string]bool, filename, field string) error {
	if !definitions[name] {
		return failure(filename, field, -1, "unknown reference")
	}
	return nil
}

// planSources нормализует все источники документа и проверяет пересечения.
func planSources(document map[string]any, pins map[string]string, pinsFile any, filename string) (map[string]any, error) {
	sources, _ := asMap(document["sources"])
	result := map[string]any{}
	destinations := []string{}
	projects := 0
	for _, name := range sortedKeys(sources) {
		field := "sources." + name
		source, _ := asMap(sources[name])
		normalized, _ := deepCopyValue(source).(map[string]any)
		destinationValue, _ := asString(source["destination"])
		destination, err := SafePath(destinationValue, field+".destination")
		if err != nil {
			return nil, failure(filename, field+".destination", -1, err.Error())
		}
		for _, existing := range destinations {
			if destination == existing || strings.HasPrefix(destination, existing+"/") ||
				strings.HasPrefix(existing, destination+"/") {
				return nil, failure(filename, field+".destination", -1, "source destinations overlap")
			}
		}
		destinations = append(destinations, destination)
		normalized["destination"] = destination
		if _, ok := normalized["gitMetadata"]; !ok {
			normalized["gitMetadata"] = false
		}
		if sourceType, _ := asString(source["type"]); sourceType == "project" {
			projects++
		} else {
			repositoryValue, _ := asString(source["repository"])
			if _, err := checked(filename, field+".repository", repositoryValue, Repository); err != nil {
				return nil, err
			}
			if pinName, ok := normalized["revisionPin"]; ok {
				pinText, _ := asString(pinName)
				revision, err := resolvePin(pinText, pins, pinsFile, filename, field+".revisionPin")
				if err != nil {
					return nil, err
				}
				if !patternHex40.MatchString(revision) {
					return nil, failure(filename, field+".revisionPin", -1,
						"expected a full lowercase 40-hex commit pin")
				}
				delete(normalized, "revisionPin")
				normalized["revision"] = revision
			}
		}
		result[name] = normalized
	}
	if projects > 1 {
		return nil, failure(filename, "sources", -1, "at most one project source is allowed")
	}
	return result, nil
}

// planBuilds подготавливает шаг сборки каждого окружения.
func planBuilds(document, sources map[string]any, pins map[string]string, pinsFile any, filename string) (map[string]any, error) {
	environments, _ := asMap(document["environments"])
	result := map[string]any{}
	for _, name := range sortedKeys(environments) {
		field := "environments." + name
		environment, _ := asMap(environments[name])
		selected, _ := environment["sources"].([]any)
		for _, item := range selected {
			sourceName, _ := asString(item)
			if _, ok := sources[sourceName]; !ok {
				return nil, failure(filename, field+".sources", -1, "unknown reference")
			}
		}
		build, _ := asMap(environment["build"])
		dockerfileValue, _ := asString(build["dockerfile"])
		dockerfile, err := SafePath(dockerfileValue, field+".build.dockerfile")
		if err != nil {
			return nil, failure(filename, field+".build.dockerfile", -1, err.Error())
		}
		underSource := false
		for _, item := range selected {
			sourceName, _ := asString(item)
			source, _ := asMap(sources[sourceName])
			destination, _ := asString(source["destination"])
			if strings.HasPrefix(dockerfile, destination+"/") {
				underSource = true
				break
			}
		}
		if !underSource {
			return nil, failure(filename, field+".build.dockerfile", -1,
				"Dockerfile must be under a selected source")
		}
		rawArguments, hasArguments := asMap(build["arguments"])
		arguments := map[string]any{}
		if hasArguments {
			for key, value := range rawArguments {
				arguments[key] = value
			}
		}
		fromPins, _ := build["argsFromPins"].([]any)
		argumentNames := make([]string, 0, len(arguments)+len(fromPins))
		for key := range arguments {
			argumentNames = append(argumentNames, key)
		}
		for _, item := range fromPins {
			key, _ := asString(item)
			argumentNames = append(argumentNames, key)
		}
		for _, key := range argumentNames {
			for _, word := range []string{"TOKEN", "PASSWORD", "SECRET", "PRIVATE_KEY", "AUTH"} {
				if strings.Contains(key, word) {
					return nil, failure(filename, field+".build", -1,
						"secret-like build argument names are forbidden")
				}
			}
		}
		for _, item := range fromPins {
			key, _ := asString(item)
			if _, exists := arguments[key]; exists {
				return nil, failure(filename, field+".build", -1, "duplicate build argument")
			}
			value, err := resolvePin(key, pins, pinsFile, filename, field+".build.argsFromPins")
			if err != nil {
				return nil, err
			}
			arguments[key] = value
		}
		result[name] = map[string]any{
			"dockerfile": dockerfile, "platform": build["platform"], "arguments": arguments,
		}
	}
	return result, nil
}

// uniqueStrings отказывает при повторе имён или путей.
func uniqueStrings(values []string, filename, field string) error {
	seen := map[string]bool{}
	for _, value := range values {
		if seen[value] {
			return failure(filename, field, -1, "duplicate names or paths")
		}
		seen[value] = true
	}
	return nil
}

// checkActor проверяет ресурсы и тома актора как _actor в Python.
func checkActor(actor map[string]any, filename, field string) error {
	resources, _ := asMap(actor["resources"])
	limits, _ := asMap(resources["limits"])
	for _, key := range sortedKeys(limits) {
		value, _ := asString(limits[key])
		number, err := strconv.ParseUint(value, 10, 64)
		if err != nil || number == 0 {
			return failure(filename, field+".resources.limits."+key, -1, "resource must fit uint64")
		}
	}
	volumes, _ := actor["volumes"].([]any)
	volumeNames := make([]string, 0, len(volumes))
	for _, item := range volumes {
		volume, _ := asMap(item)
		name, _ := asString(volume["name"])
		volumeNames = append(volumeNames, name)
	}
	if err := uniqueStrings(volumeNames, filename, field+".volumes"); err != nil {
		return err
	}
	containers, _ := actor["containers"].([]any)
	for _, item := range containers {
		container, _ := asMap(item)
		env, _ := container["env"].([]any)
		envNames := make([]string, 0, len(env))
		for _, entry := range env {
			pair, _ := asMap(entry)
			name, _ := asString(pair["name"])
			envNames = append(envNames, name)
		}
		if err := uniqueStrings(envNames, filename, field+".containers.env"); err != nil {
			return err
		}
		mounts, _ := container["volumeMounts"].([]any)
		mountNames := make([]string, 0, len(mounts))
		paths := make([]string, 0, len(mounts))
		for _, entry := range mounts {
			mount, _ := asMap(entry)
			name, _ := asString(mount["name"])
			known := false
			for _, volumeName := range volumeNames {
				if name == volumeName {
					known = true
					break
				}
			}
			if !known {
				return failure(filename, field+".containers.volumeMounts.name", -1, "unknown reference")
			}
			mountNames = append(mountNames, name)
			path, _ := asString(mount["mountPath"])
			if strings.Contains(path, "\\") || hasControl(path) || hasDotDotSegment(path) {
				return failure(filename, field+".containers.volumeMounts.mountPath", -1,
					"unsafe absolute mount path")
			}
			paths = append(paths, cleanPosix(path))
		}
		if err := uniqueStrings(mountNames, filename, field+".containers.volumeMounts"); err != nil {
			return err
		}
		if err := uniqueStrings(paths, filename, field+".containers.volumeMounts.mountPath"); err != nil {
			return err
		}
	}
	return nil
}

// checkDefinitions проверяет все определения документа, включая невыбранные.
func checkDefinitions(document map[string]any, filename string) error {
	networks, _ := asMap(document["networks"])
	for _, name := range sortedKeys(networks) {
		network, _ := asMap(networks[name])
		for _, protocol := range sortedKeys(network) {
			rule, _ := asMap(network[protocol])
			hosts, _ := rule["hosts"].([]any)
			for _, item := range hosts {
				host, _ := asString(item)
				if _, err := PublicHostname(host); err != nil {
					return failure(filename, "networks."+name+"."+protocol+".hosts", -1, err.Error())
				}
			}
		}
	}
	environments, _ := asMap(document["environments"])
	templates, _ := asMap(document["templates"])
	for _, name := range sortedKeys(templates) {
		field := "templates." + name
		template, _ := asMap(templates[name])
		environment, _ := asString(template["environment"])
		if _, ok := environments[environment]; !ok {
			return failure(filename, field+".environment", -1, "unknown reference")
		}
		networkName, _ := asString(template["network"])
		if _, ok := networks[networkName]; !ok {
			return failure(filename, field+".network", -1, "unknown reference")
		}
		actor, _ := asMap(template["actor"])
		if err := checkActor(actor, filename, field+".actor"); err != nil {
			return err
		}
	}
	targets, _ := asMap(document["targets"])
	for _, name := range sortedKeys(targets) {
		target, _ := asMap(targets[name])
		templateName, _ := asString(target["template"])
		if _, ok := templates[templateName]; !ok {
			return failure(filename, "targets."+name+".template", -1, "unknown reference")
		}
	}
	defaultTarget, _ := asString(document["defaultTarget"])
	if _, ok := targets[defaultTarget]; !ok {
		return failure(filename, "defaultTarget", -1, "unknown reference")
	}
	return nil
}

// Workflow returns a named workflow from a validated Broodfile document.
func Workflow(document map[string]any, name, filename string) (*pipeline.Document, error) {
	workflows, _ := asMap(document["workflows"])
	graph, ok := asMap(workflows[name])
	if !ok {
		return nil, fmt.Errorf("%s: unknown workflow %q (available: %s)", filename, name, strings.Join(sortedKeys(workflows), ", "))
	}
	copyGraph := make(map[string]any, len(graph)+2)
	for key, value := range graph {
		copyGraph[key] = value
	}
	copyGraph["apiVersion"] = "brood.dev/v1alpha1"
	copyGraph["name"] = name
	return pipeline.ParseEmbedded(copyGraph, filename+": workflows."+name)
}

// WorkflowSnapshots returns validated standalone YAML for declared workflows.
func WorkflowSnapshots(document map[string]any, filename string) (map[string]string, error) {
	workflows, _ := asMap(document["workflows"])
	result := make(map[string]string, len(workflows))
	for _, name := range sortedKeys(workflows) {
		graph, err := Workflow(document, name, filename)
		if err != nil {
			return nil, err
		}
		data, err := yaml.Marshal(graph)
		if err != nil {
			return nil, fmt.Errorf("%s: workflow %q: could not encode snapshot", filename, name)
		}
		if _, err := pipeline.Parse(string(data), filename+": workflows."+name); err != nil {
			return nil, err
		}
		result[name] = string(data)
	}
	return result, nil
}

// validateWorkflows использует общий validator конвейеров: узлы workflow
// называют роли, а не шаблоны, сверка со списком templates не нужна (#208).
func validateWorkflows(document map[string]any, filename string) error {
	workflows, _ := asMap(document["workflows"])
	for _, name := range sortedKeys(workflows) {
		parsed, err := Workflow(document, name, filename)
		if err != nil {
			return err
		}
		if err := parsed.Validate(); err != nil {
			return fmt.Errorf("%s: workflows.%s: %w", filename, name, err)
		}
	}
	return nil
}

// planProjectConfig сохраняет присутствие необязательных полей серверного проекта.
func planProjectConfig(document map[string]any, filename string) (map[string]any, error) {
	config := map[string]any{"name": document["name"], "repository": ""}
	declaration, _ := asMap(document["project"])
	if repository, ok := declaration["repository"].(string); ok && repository != "" {
		canonical, err := checked(filename, "project.repository", repository, gitorigin.FromRemote)
		if err != nil {
			return nil, err
		}
		config["repository"] = canonical
	}
	if description, present := declaration["description"]; present {
		if hasControl(description.(string)) {
			return nil, failure(filename, "project.description", -1, "control characters are forbidden")
		}
		config["description"] = description
	}
	if capacity, present := declaration["capacity"]; present {
		limits, _ := asMap(capacity)
		for _, key := range []string{"max_swarms", "max_actors", "max_cpu_millicores", "max_memory_bytes"} {
			if text, ok := limits[key].(string); ok {
				if _, err := strconv.ParseInt(text, 10, 64); err != nil {
					return nil, failure(filename, "project.capacity."+key, -1, "expected a nonnegative decimal string within int64")
				}
			}
		}
		config["capacity"] = deepCopyValue(capacity)
	}
	if reference, present := declaration["git_credential"]; present {
		name, _ := reference.(string)
		if repository, explicit := declaration["repository"].(string); explicit && repository == "" && name != "" {
			return nil, failure(filename, "project.git_credential", -1, "a Git credential reference requires a project repository")
		}
		config["git_credential"] = name
	}
	return config, nil
}

// MakePlan проверяет весь документ и возвращает независимый план выбранной цели.
func MakePlan(document map[string]any, pins map[string]string, target string,
	registry string, atespace string, filename string) (map[string]any, error) {
	if err := validateSchema(document, filename, nil); err != nil {
		return nil, err
	}
	if err := validateWorkflows(document, filename); err != nil {
		return nil, err
	}
	projectConfig, err := planProjectConfig(document, filename)
	if err != nil {
		return nil, err
	}
	resolvedPins, err := pinValues(pins, filename)
	if err != nil {
		return nil, err
	}
	var pinsFile any
	if raw, ok := document["pinsFile"]; ok && raw != nil {
		value, _ := asString(raw)
		path, err := SafePath(value, "pinsFile")
		if err != nil {
			return nil, failure(filename, "pinsFile", -1, err.Error())
		}
		pinsFile = path
	}
	sources, err := planSources(document, resolvedPins, pinsFile, filename)
	if err != nil {
		return nil, err
	}
	builds, err := planBuilds(document, sources, resolvedPins, pinsFile, filename)
	if err != nil {
		return nil, err
	}
	if err := checkDefinitions(document, filename); err != nil {
		return nil, err
	}
	if target == "" {
		target, _ = asString(document["defaultTarget"])
	}
	targets, _ := asMap(document["targets"])
	if _, ok := targets[target]; !ok {
		return nil, failure(filename, "target", -1, "unknown reference")
	}
	if registry == "" {
		return nil, failure(filename, "registry", -1, "BUILD_REGISTRY is required")
	}
	registry, err = checked(filename, "registry", registry, registryPrefix)
	if err != nil {
		return nil, err
	}
	if atespace == "" {
		return nil, failure(filename, "atespace", -1, "BUILD_ATESPACE is required")
	}
	if utf8.RuneCountInString(atespace) > 63 || !patternAtespace.MatchString(atespace) {
		return nil, failure(filename, "atespace", -1, "BUILD_ATESPACE must be a DNS label")
	}
	templates, _ := asMap(document["templates"])
	targetEntry, _ := asMap(targets[target])
	templateName, _ := asString(targetEntry["template"])
	template, _ := asMap(templates[templateName])
	environment, _ := asString(template["environment"])
	environments, _ := asMap(document["environments"])
	selected, _ := asMap(environments[environment])
	selectedSources, _ := selected["sources"].([]any)
	selectedBuild, _ := asMap(selected["build"])
	usedPinNames := map[string]bool{}
	for _, item := range asList(selectedBuild["argsFromPins"]) {
		if name, ok := asString(item); ok {
			usedPinNames[name] = true
		}
	}
	documentSources, _ := asMap(document["sources"])
	for _, item := range selectedSources {
		sourceName, _ := asString(item)
		source, _ := asMap(documentSources[sourceName])
		if pinName, ok := asString(source["revisionPin"]); ok {
			usedPinNames[pinName] = true
		}
	}
	planSourcesMap := map[string]any{}
	for _, item := range selectedSources {
		sourceName, _ := asString(item)
		planSourcesMap[sourceName] = sources[sourceName]
	}
	usedPins := map[string]any{}
	for name := range usedPinNames {
		usedPins[name] = resolvedPins[name]
	}
	networks, _ := asMap(document["networks"])
	networkName, _ := asString(template["network"])
	return map[string]any{
		"apiVersion": document["apiVersion"], "project": document["name"],
		"projectConfig": projectConfig,
		"target":        target, "environment": environment,
		"sources": planSourcesMap, "build": builds[environment],
		"actor":    deepCopyValue(template["actor"]),
		"network":  deepCopyValue(networks[networkName]),
		"registry": registry,
		"atespace": atespace,
		"pins":     usedPins,
		"pinsFile": pinsFile,
	}, nil
}
