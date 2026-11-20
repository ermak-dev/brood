package build

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"brood.dev/go/brood/projectsecret"
)

// maxDotenvBytes с запасом покрывает предел окружения проекта.
const maxDotenvBytes = 4 * projectsecret.MaxTotalEnvBytes

// ParseDotenv reads KEY=VALUE lines. Blank lines and # comments are skipped,
// a leading "export " is accepted, and a value may be wrapped in single or
// double quotes; inside double quotes \" and \\ are unescaped. An unquoted
// value ends at a # preceded by whitespace. Errors name file and the line
// number and never include any part of the line.
func ParseDotenv(file string, data []byte) (map[string]string, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("%s: expected UTF-8 text", file)
	}
	vars := map[string]string{}
	text := strings.TrimPrefix(string(data), "\xef\xbb\xbf")
	for index, line := range strings.Split(text, "\n") {
		number := index + 1
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, found := strings.CutPrefix(line, "export"); found && rest != strings.TrimLeft(rest, " \t") {
			line = strings.TrimLeft(rest, " \t")
		}
		name, raw, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("%s: line %d: expected KEY=VALUE", file, number)
		}
		name = strings.TrimRight(name, " \t")
		value, reason := dotenvValue(raw)
		if reason == "" {
			if err := projectsecret.ValidateVar(name, value); err != nil {
				reason = err.Error()
			} else if _, duplicate := vars[name]; duplicate {
				reason = "duplicate variable name"
			}
		}
		if reason != "" {
			return nil, fmt.Errorf("%s: line %d: %s", file, number, reason)
		}
		vars[name] = value
	}
	if err := projectsecret.ValidateBatch(nil, vars); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return vars, nil
}

// dotenvValue снимает кавычки и хвостовой комментарий; reason не содержит
// значения.
func dotenvValue(raw string) (value, reason string) {
	if trimmed := strings.TrimLeft(raw, " \t"); trimmed == "" || (trimmed[0] != '"' && trimmed[0] != '\'') {
		for i := 1; i < len(raw); i++ {
			if raw[i] == '#' && (raw[i-1] == ' ' || raw[i-1] == '\t') {
				raw = raw[:i]
				break
			}
		}
		return strings.Trim(raw, " \t"), ""
	}
	raw = strings.TrimLeft(raw, " \t")
	quote := raw[0]
	var result strings.Builder
	for i := 1; i < len(raw); i++ {
		char := raw[i]
		switch {
		case quote == '"' && char == '\\' && i+1 < len(raw) && (raw[i+1] == '"' || raw[i+1] == '\\'):
			i++
			result.WriteByte(raw[i])
		case char == quote:
			rest := strings.TrimLeft(raw[i+1:], " \t")
			if rest != "" && !strings.HasPrefix(rest, "#") {
				return "", "unexpected text after the closing quote"
			}
			return result.String(), ""
		default:
			result.WriteByte(char)
		}
	}
	return "", "unterminated quoted value"
}

// readSource читает обычный файл по пути относительно корня репозитория.
// Симлинк допустим, пока ведёт внутрь корня; os.Root держит ту же границу
// при открытии, если дерево подменят после проверки.
func readSource(directory, source string, limit int) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(directory, filepath.FromSlash(source)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("file does not exist")
		}
		return nil, errors.New("file is inaccessible")
	}
	relative, err := filepath.Rel(directory, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, errors.New("path resolves through a symlink outside the repository root")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, errors.New("could not open the repository root")
	}
	defer root.Close()
	file, err := root.Open(filepath.FromSlash(source))
	if err != nil {
		return nil, errors.New("could not open the file inside the repository root")
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("expected a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, errors.New("could not read the file")
	}
	if len(data) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

// readEnv читает dotenv-файл декларации; ошибки называют файл и строку.
func (p *Project) readEnv() (map[string]string, error) {
	data, err := readSource(p.Root, p.Secrets.Env, maxDotenvBytes)
	if err != nil {
		return nil, failure("brood.secrets.env", p.Secrets.envLine, p.Secrets.Env+": "+err.Error())
	}
	return ParseDotenv(p.Secrets.Env, data)
}

// ReadSecrets reads the declared secret values from the checkout: variables
// from the dotenv file and files from their sources, named by destination
// path. It validates the whole batch against the project secret limits. The
// project must come from Load and declare a secrets block. Errors never
// include values or file contents.
func (p *Project) ReadSecrets() ([]projectsecret.File, map[string]string, error) {
	if p.Secrets == nil || p.Root == "" {
		return nil, nil, errors.New("no secrets block is loaded from " + Filename)
	}
	vars := map[string]string{}
	if p.Secrets.Env != "" {
		var err error
		if vars, err = p.readEnv(); err != nil {
			return nil, nil, err
		}
	}
	files := make([]projectsecret.File, 0, len(p.Secrets.Files))
	for i, declared := range p.Secrets.Files {
		data, err := readSource(p.Root, declared.Source, projectsecret.MaxFileBytes)
		if err != nil {
			return nil, nil, failure(fmt.Sprintf("brood.secrets.files[%d].source", i), declared.line, declared.Source+": "+err.Error())
		}
		files = append(files, projectsecret.File{Name: declared.Path, Data: data})
	}
	if err := projectsecret.ValidateBatch(files, vars); err != nil {
		return nil, nil, failure("brood.secrets", 0, err.Error())
	}
	return files, vars, nil
}

// Plan returns the resolved declaration as printed by bro build --plan. Of
// the secrets it carries names only: destination paths, local source paths
// and the variable names of the dotenv file when that file exists.
func (p *Project) Plan() (map[string]any, error) {
	plan := map[string]any{
		"file":    Filename,
		"project": p.Config,
		"image":   map[string]any{"playbook": Filename, "tasks": p.HasTasks},
		"secrets": nil,
	}
	if p.Secrets == nil {
		return plan, nil
	}
	secrets := map[string]any{"env": nil}
	if p.Secrets.Env != "" {
		env := map[string]any{"source": p.Secrets.Env, "present": false, "variables": []string{}}
		if _, err := os.Stat(filepath.Join(p.Root, filepath.FromSlash(p.Secrets.Env))); p.Root != "" && err == nil {
			vars, err := p.readEnv()
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(vars))
			for name := range vars {
				names = append(names, name)
			}
			sort.Strings(names)
			env["present"], env["variables"] = true, names
		}
		secrets["env"] = env
	}
	files := make([]map[string]string, 0, len(p.Secrets.Files))
	for _, file := range p.Secrets.Files {
		files = append(files, map[string]string{"path": file.Path, "source": file.Source})
	}
	secrets["files"] = files
	plan["secrets"] = secrets
	return plan, nil
}
