// Минимальный разбор аргументов командлета: позиционные по порядку, флаги
// --key value / --key=value, повторяемые и булевы. Разбор повторяет
// argparse ровно настолько, насколько нужно командлетам brood, и не знает
// ни одного имени флага: имена объявляет командлет.
package cli

import "fmt"

// Flags — разобранные аргументы: позиционные и значения по имени флага.
type Flags struct {
	Positional []string
	values     map[string]string
	repeated   map[string][]string
	bools      map[string]bool
}

// ParseFlags разбирает argv. spec описывает флаги со значением, repeated —
// повторяемые, boolFlags — булевы (присутствие = true).
func ParseFlags(argv []string, spec, repeated, boolFlags []string) *Flags {
	f := &Flags{
		values: map[string]string{}, repeated: map[string][]string{}, bools: map[string]bool{},
	}
	wants := func(name string) (kind string) {
		for _, s := range spec {
			if s == name {
				return "value"
			}
		}
		for _, s := range repeated {
			if s == name {
				return "repeated"
			}
		}
		for _, s := range boolFlags {
			if s == name {
				return "bool"
			}
		}
		return ""
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if len(a) < 2 || a[:2] != "--" {
			f.Positional = append(f.Positional, a)
			continue
		}
		name, inline, hasInline := cut2(a, "=")
		kind := wants(name)
		switch kind {
		case "":
			Fail("unknown flag %s", name)
		case "bool":
			f.bools[name] = true
		default:
			value := inline
			if !hasInline {
				if i+1 >= len(argv) {
					Fail("%s needs a value", name)
				}
				i++
				value = argv[i]
			}
			if kind == "repeated" {
				f.repeated[name] = append(f.repeated[name], value)
			} else {
				f.values[name] = value
			}
		}
	}
	return f
}

func cut2(s, sep string) (before, after string, found bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

// String — значение флага, "" если не задан.
func (f *Flags) String(name string) string { return f.values[name] }

// Int — целочисленное значение флага; def, если не задан или не целое.
func (f *Flags) Int(name string, def int) int {
	v := f.values[name]
	if v == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		Fail("%s: %q is not a number", name, v)
	}
	return n
}

// Repeated — все значения повторяемого флага.
func (f *Flags) Repeated(name string) []string { return f.repeated[name] }

// Bool — был ли булев флаг.
func (f *Flags) Bool(name string) bool { return f.bools[name] }

// Pos — позиционный аргумент номер i, "" если нет.
func (f *Flags) Pos(i int) string {
	if i < len(f.Positional) {
		return f.Positional[i]
	}
	return ""
}
