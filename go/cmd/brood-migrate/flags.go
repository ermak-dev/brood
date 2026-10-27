// Строгий разбор флагов и файлов офлайн-импорта.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type flags struct{ values map[string]string }

func parseFlags(argv []string, names ...string) (*flags, error) {
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}
	f := &flags{values: map[string]string{}}
	for i := 0; i < len(argv); i += 2 {
		name := strings.TrimPrefix(argv[i], "--")
		if !strings.HasPrefix(argv[i], "--") || !known[name] {
			return nil, fmt.Errorf("unknown argument %q", argv[i])
		}
		if _, ok := f.values[name]; ok {
			return nil, fmt.Errorf("duplicate flag --%s", name)
		}
		if i+1 >= len(argv) || argv[i+1] == "" || strings.HasPrefix(argv[i+1], "--") {
			return nil, fmt.Errorf("flag --%s needs a value", name)
		}
		f.values[name] = argv[i+1]
	}
	return f, nil
}
func (f *flags) get(name string) (string, error) {
	if v := f.values[name]; v != "" {
		return v, nil
	}
	return "", fmt.Errorf("missing required flag --%s", name)
}
func existingTargetFlag(f *flags) (bool, error) {
	v := f.values["existing-target"]
	if v != "" && v != "yes" {
		return false, fmt.Errorf("--existing-target accepts only yes")
	}
	return v == "yes", nil
}

func readJSON(path string, dst any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// encoding/json иначе молча принимает повторённые ключи.
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := checkJSONValue(d); err != nil {
		return fmt.Errorf("read JSON %s: %w", path, err)
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("JSON must contain exactly one value")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("read JSON %s: %w", path, err)
	}
	return nil
}

func checkJSONValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return fmt.Errorf("invalid object key")
			}
			if keys[name] {
				return fmt.Errorf("duplicate JSON key %q", name)
			}
			keys[name] = true
			if err := checkJSONValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := checkJSONValue(d); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected delimiter")
	}
	_, err = d.Token()
	return err
}

// writeReport не перезаписывает существующий файл, даже по символьной ссылке.
func writeReport(path string, report any) (err error) {
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create new report: %w", err)
	}
	defer func() {
		f.Close()
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err = f.Write(append(raw, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return f.Close()
}
