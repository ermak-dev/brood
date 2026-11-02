// Package projectsecret defines bounded project secrets shared by storage and delivery.
// It performs no I/O and never includes supplied inputs in validation errors.
package projectsecret

import (
	"errors"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxFileBytes      = 512 * 1024
	MaxTotalFileBytes = 2 * 1024 * 1024
	MaxVarBytes       = 16 * 1024
	MaxTotalEnvBytes  = 64 * 1024
	MaxFiles          = 128
	MaxVars           = 128
)

// File is a relative workspace path and its opaque bytes.
type File struct {
	Name string `json:"name"`
	Data []byte `json:"data"`
}

// Bundle is sensitive internal delivery data, not an operator-facing response.
type Bundle struct {
	Revision int64             `json:"revision"`
	Files    []File            `json:"files"`
	Vars     map[string]string `json:"vars"`
}

// FileInfo describes a stored file without disclosing its contents.
type FileInfo struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// Metadata contains only names, sizes and the project's secret revision.
type Metadata struct {
	Revision int64      `json:"revision"`
	Files    []FileInfo `json:"files"`
	Vars     []string   `json:"vars"`
}

var varName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateFileName requires a canonical relative POSIX path without runtime controls.
// Reserved components are rejected at every depth, including the component itself.
// A secret .gitignore could negate the excludes protecting other delivered files.
func ValidateFileName(name string) error {
	if name == "" || len(name) > 1024 || strings.IndexFunc(name, unicode.IsControl) >= 0 || !utf8.ValidString(name) || path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\:\x00\r\n") {
		return errors.New("invalid secret file path")
	}
	for _, part := range strings.Split(name, "/") {
		if len(part) > 255 {
			return errors.New("secret file path component exceeds byte limit")
		}
		switch strings.ToLower(part) {
		case "", ".", "..", ".git", ".gitignore", ".claude", ".mcp.json", "claude.md":
			return errors.New("invalid or reserved secret file path")
		}
	}
	return nil
}

// ValidateVar checks syntax, single-line values and fixed guest execution policy.
func ValidateVar(name, value string) error {
	if !varName.MatchString(name) {
		return errors.New("invalid secret variable name")
	}
	upper := strings.ToUpper(name)
	for _, prefix := range []string{"BROOD_", "CLAUDE_", "ANTHROPIC_", "LD_", "DYLD_", "PYTHON", "BASH_FUNC_", "GIT_CONFIG_"} {
		if strings.HasPrefix(upper, prefix) {
			return errors.New("reserved secret variable name")
		}
	}
	switch upper {
	case "HOME", "PWD", "OLDPWD", "PATH", "USER", "LOGNAME", "SHELL", "ENV", "BASH_ENV", "SHELLOPTS", "BASHOPTS", "IFS", "CDPATH", "GLOBIGNORE", "PROMPT_COMMAND", "PS4", "NODE_OPTIONS", "NODE_PATH", "PERL5OPT", "PERL5LIB", "RUBYOPT", "RUBYLIB", "GCONV_PATH", "LOCPATH", "GLIBC_TUNABLES", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "FTP_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "NODE_EXTRA_CA_CERTS", "GIT_PROXY_COMMAND", "GIT_SSH", "GIT_SSH_COMMAND", "GIT_SSL_NO_VERIFY", "ZDOTDIR":
		return errors.New("reserved secret variable name")
	}
	if !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("secret variable value must be one line without NUL")
	}
	if len(value) > MaxVarBytes {
		return errors.New("secret variable value exceeds byte limit")
	}
	return nil
}

// ValidateBatch checks a complete candidate collection, including aggregate bounds.
// Environment size counts each NAME=VALUE followed by its terminating NUL byte.
func ValidateBatch(files []File, vars map[string]string) error {
	if len(files) > MaxFiles || len(vars) > MaxVars {
		return errors.New("secret count exceeds limit")
	}
	total := 0
	names := make(map[string]bool, len(files))
	for _, file := range files {
		if err := ValidateFileName(file.Name); err != nil {
			return err
		}
		if len(file.Data) > MaxFileBytes {
			return errors.New("secret file exceeds byte limit")
		}
		total += len(file.Data)
		if total > MaxTotalFileBytes {
			return errors.New("secret files exceed total byte limit")
		}
		if names[file.Name] {
			return errors.New("duplicate secret file path")
		}
		names[file.Name] = true
	}
	for name := range names {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if names[parent] {
				return errors.New("conflicting secret file paths")
			}
		}
	}
	total = 0
	for name, value := range vars {
		if err := ValidateVar(name, value); err != nil {
			return err
		}
		if len(name) > MaxTotalEnvBytes {
			return errors.New("secret environment exceeds total byte limit")
		}
		total += len(name) + len(value) + 2
		if total > MaxTotalEnvBytes {
			return errors.New("secret environment exceeds total byte limit")
		}
	}
	return nil
}

// ValidateBundle checks delivery data without changing or normalizing it.
func ValidateBundle(bundle Bundle) error {
	if bundle.Revision < 0 {
		return errors.New("invalid secret revision")
	}
	return ValidateBatch(bundle.Files, bundle.Vars)
}
