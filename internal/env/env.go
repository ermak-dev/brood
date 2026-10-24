// Пакет env читает .env установки: KEY=VALUE построчно, повторы и
// комментарии пропускаются, кавычки с краёв значения срезаются. Формат
// намеренно примитивен: его читает только brood.
package env

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var (
	once   sync.Once
	loaded map[string]string
)

// Load читает .env один раз за процесс. Отсутствие файла — штатный случай:
// на чужой машине без .env работает всё, что трекера не касается.
func Load(path string) {
	once.Do(func() {
		loaded = map[string]string{}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return
			}
			// Глотание ошибки чтения превращало существующий, но
			// недоступный файл в «настроек нет» — настройка молча ехала
			// в дефолт. Отказ с путём, без содержимого файла.
			fmt.Fprintf(os.Stderr, "brood: cannot read %s: %v\n", path, err)
			os.Exit(1)
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			loaded[strings.TrimSpace(key)] = stripQuotes(strings.TrimSpace(value))
		}
	})
}

// stripQuotes снимает парные кавычки с краёв: .env mop пишет значения
// в одинарных кавычках, и кавычка — часть формата, не значения.
func stripQuotes(v string) string {
	if len(v) >= 2 {
		for _, q := range []byte{'\'', '"'} {
			if v[0] == q && v[len(v)-1] == q {
				return v[1 : len(v)-1]
			}
		}
	}
	return v
}

// Get — значение настройки: окружение старше .env (точечная проба без
// правки файла), затем .env.
func Get(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return loaded[name]
}

// EnvPath — путь .env: рядом с бинарием его нет, ищем от текущего каталога
// вверх, как рабочую копию. Находит и корень репозитория, откуда берётся
// git origin.
func EnvPath() string {
	dir, err := os.Getwd()
	if err != nil {
		return ".env"
	}
	for {
		candidate := filepath.Join(dir, ".env")
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ".env"
		}
		dir = parent
	}
}
