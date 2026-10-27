// Новый переносимый Docker-контекст из явно выбранных Git-источников.
// Порт brood/dev/build/context.py (issue #35).
package build

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// encodeJSONText пишет значение как json.dumps(sort_keys=True, indent=2) в Python:
// ключи отсортированы, отступ два пробела, ASCII выведен эскейпами, NaN запрещён.
func encodeJSONText(value any) (string, error) {
	var builder strings.Builder
	if err := writeJSON(&builder, value, 0, true); err != nil {
		return "", err
	}
	return builder.String(), nil
}

// writeJSON печатает значение; indent=true включает режим PrettyPrint.
func writeJSON(builder *strings.Builder, value any, depth int, indent bool) error {
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
		text, err := pythonJSONNumber(float64(typed))
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
		if len(typed) == 0 {
			builder.WriteString("[]")
			return nil
		}
		builder.WriteString("[")
		if indent {
			builder.WriteString("\n")
		}
		for index, item := range typed {
			if index > 0 {
				builder.WriteString(",")
				if indent {
					builder.WriteString("\n")
				}
			}
			if indent {
				builder.WriteString(strings.Repeat("  ", depth+1))
			}
			if err := writeJSON(builder, item, depth+1, indent); err != nil {
				return err
			}
		}
		if indent {
			builder.WriteString("\n" + strings.Repeat("  ", depth))
		}
		builder.WriteString("]")
	case map[string]any:
		if len(typed) == 0 {
			builder.WriteString("{}")
			return nil
		}
		builder.WriteString("{")
		if indent {
			builder.WriteString("\n")
		}
		for index, key := range sortedKeys(typed) {
			if index > 0 {
				builder.WriteString(",")
				if indent {
					builder.WriteString("\n")
				}
			}
			if indent {
				builder.WriteString(strings.Repeat("  ", depth+1))
			}
			encoded, err := pythonJSONString(key)
			if err != nil {
				return err
			}
			builder.WriteString(encoded)
			if indent {
				builder.WriteString(": ")
			} else {
				builder.WriteString(":")
			}
			if err := writeJSON(builder, typed[key], depth+1, indent); err != nil {
				return err
			}
		}
		if indent {
			builder.WriteString("\n" + strings.Repeat("  ", depth))
		}
		builder.WriteString("}")
	default:
		// Значения плана всегда json-подобны; остальное — отказ без паники.
		return fmt.Errorf("value is not JSON serializable")
	}
	return nil
}

// pythonJSONString кодирует строку как json.dumps с ensure_ascii=True.
func pythonJSONString(value string) (string, error) {
	for _, char := range value {
		if char < 32 || char == 127 {
			// Управляющие и DEL в JSON-значениях плана не встречаются.
			return "", fmt.Errorf("value is not JSON serializable")
		}
	}
	var builder strings.Builder
	builder.WriteByte('"')
	for _, char := range value {
		switch {
		case char == '"':
			builder.WriteString("\\\"")
		case char == '\\':
			builder.WriteString("\\\\")
		case char < 128:
			builder.WriteRune(char)
		default:
			// Non-ASCII печатается эскейпами \uXXXX, как ensure_ascii=True.
			for _, unit := range []rune{char} {
				if unit >= 0x10000 {
					// Суррогатная пара, как в Python.
					encoded := unit - 0x10000
					builder.WriteString(fmt.Sprintf("\\u%04x\\u%04x",
						0xd800+(encoded>>10), 0xdc00+(encoded&0x3ff)))
				} else {
					builder.WriteString(fmt.Sprintf("\\u%04x", unit))
				}
			}
		}
	}
	builder.WriteByte('"')
	return builder.String(), nil
}

// pythonJSONNumber печатает число как repr Python: целое без точки,
// дробное с кратчайшей записью.
func pythonJSONNumber(value float64) (string, error) {
	if value != value || value > maxJSONFloat || value < -maxJSONFloat {
		return "", fmt.Errorf("value is not JSON serializable")
	}
	if value >= minJSONInt && value <= maxJSONInt && value == float64(int64(value)) {
		return fmt.Sprintf("%d", int64(value)), nil
	}
	return strconv.FormatFloat(value, 'g', -1, 64), nil
}

const (
	maxJSONFloat = 1.7976931348623157e308
	maxJSONInt   = 9223372036854775807
	minJSONInt   = -9223372036854775808
)

// externalSource подготавливает внешний git-источник: кэш или свежий fetch.
func externalSource(spec map[string]any, target string, cache map[string]string) (map[string]any, error) {
	url, _ := spec["repository"].(string)
	revision, _ := spec["revision"].(string)
	gitMetadata, _ := spec["gitMetadata"].(bool)
	if path, ok := cache[url]; ok {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("BUILD_SOURCE_CACHE paths must be absolute")
		}
		remote, present, err := Origin(path, true)
		if err != nil {
			return nil, err
		}
		if !present || remote != url {
			return nil, fmt.Errorf("BUILD_SOURCE_CACHE checkout origin does not match its repository")
		}
		return Snapshot(path, revision, target, gitMetadata)
	}
	directory, err := os.MkdirTemp("", "brood-git-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	if _, err := Git(directory, "init", "-q", "--template="); err != nil {
		return nil, err
	}
	remote, err := Repository(url)
	if err != nil {
		return nil, err
	}
	if _, err := Git(directory, "remote", "add", "origin", remote); err != nil {
		return nil, err
	}
	if _, err := Git(directory, "-c", "core.hooksPath=/dev/null", "fetch", "-q", "--no-tags",
		"--depth=1", "origin", revision); err != nil {
		return nil, err
	}
	return Snapshot(directory, revision, target, gitMetadata)
}

// Export публикует контекст целиком; при ошибке целевой путь не появляется.
func Export(project *Project, destination string, cache map[string]string) (map[string]any, error) {
	if cache == nil {
		cache = map[string]string{}
	}
	target, err := filepath.Abs(destination)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(target); err == nil {
		return nil, fmt.Errorf("Build context destination already exists")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(target), ".brood-context-")
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(staging)
		}
	}()
	reports := map[string]any{}
	portable := map[string]any{}
	plan := project.Plan
	for _, key := range []string{"apiVersion", "project", "environment", "sources", "build", "pins", "pinsFile"} {
		portable[key] = deepCopyValue(plan[key])
	}
	planSources, _ := plan["sources"].(map[string]any)
	portableSources := map[string]any{}
	for _, name := range sortedKeys(planSources) {
		spec, _ := planSources[name].(map[string]any)
		path := filepath.Join(staging, spec["destination"].(string))
		var report map[string]any
		if spec["type"] == "project" {
			report, err = Snapshot(project.Root, spec["revision"].(string), path, spec["gitMetadata"].(bool))
		} else {
			report, err = externalSource(spec, path, cache)
		}
		if err != nil {
			return nil, err
		}
		// Как {**spec, **report}: поля отчёта перекрывают объявление.
		reportValue := map[string]any{}
		for key, value := range spec {
			reportValue[key] = deepCopyValue(value)
		}
		for key, value := range report {
			reportValue[key] = value
		}
		reports[name] = reportValue
		portableSources[name] = reportValue
	}
	portable["sources"] = portableSources
	build, _ := plan["build"].(map[string]any)
	dockerfile := filepath.Join(staging, build["dockerfile"].(string))
	info, err := os.Stat(dockerfile)
	if err != nil || info.IsDir() {
		return nil, fmt.Errorf("Dockerfile is missing from the selected source commit")
	}
	resolved, err := filepath.EvalSymlinks(dockerfile)
	if err != nil {
		return nil, err
	}
	stagingRoot, err := filepath.EvalSymlinks(staging)
	if err != nil {
		return nil, err
	}
	if resolved != stagingRoot && !strings.HasPrefix(resolved, stagingRoot+string(filepath.Separator)) {
		return nil, fmt.Errorf("Dockerfile is missing from the selected source commit")
	}
	text, err := encodeJSONText(portable)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(staging, "build-plan.json"), []byte(text+"\n"), 0o644); err != nil {
		return nil, err
	}
	if err := os.Rename(staging, target); err != nil {
		return nil, err
	}
	committed = true
	return reports, nil
}
