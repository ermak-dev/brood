// Чистая логика трекера: метки, эпики, ветки. Проверяется без GitLab
// (tests в этом же пакете), GitLab-запросы её только вызывают.
package gitlab

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Словарь меток. Три группы — scoped labels GitLab: у задачи ровно одна из
// каждой, вторая молча вытесняет первую. Значения проверяются здесь, а не
// в GitLab: опечатка в метке заводит новую метку, и задача пропадает из
// выборки.
var vocab = map[string][]string{
	"status":    {"live", "wip", "fixed", "noise", "dup", "parked"},
	"sev":       {"high", "med", "low"},
	"component": {"bus", "nomad", "agent", "session", "puppets", "driver", "deploy", "mcp", "master", "cli", "docs"},
	"type":      {"bug", "feature", "refactor", "docs", "ci", "epic"},
}

// Незакрытые состояния: задача в очереди (live), взята в работу (wip)
// либо отложена (parked) — отложенная открыта, но не в работе; закрытая
// отложенная пропадает из списка и выглядит сделанной.
var openStatuses = []string{"live", "wip", "parked"}
var closeStatuses = []string{"fixed", "noise", "dup"}

// CloseStatuses — словарь закрывающих статусов наружу (choices командлета).
func CloseStatuses() []string { return closeStatuses }

// OpenStatuses — словарь открытых статусов наружу.
func OpenStatuses() []string { return openStatuses }

func has(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

var sevOrder = map[string]int{"sev::high": 0, "sev::med": 1, "sev::low": 2}

// branchTypes — префикс ветки по метке типа.
var branchTypes = map[string]string{
	"bug": "bug", "feature": "feat", "refactor": "refactor",
	"docs": "docs", "ci": "ci",
}

// CheckLabel — метка из словаря либо громкий отказ с перечнем. Опечатка в
// scoped-метке не ошибка для GitLab: он заведёт новую метку, и задача
// просто выпадет из выборок, которыми её ищут.
func CheckLabel(name string) (string, error) {
	group, value, _ := strings.Cut(name, "::")
	allowed, ok := vocab[group]
	if !ok {
		return "", fmt.Errorf("unknown label group %s; one of: %s", group, vocabKeys())
	}
	if !has(allowed, value) {
		return "", fmt.Errorf("unknown %s '%s'; one of: %s", group, value, strings.Join(allowed, ", "))
	}
	return name, nil
}

// Vocab — значения группы меток наружу (справка labels, choices).
func Vocab(group string) []string { return vocab[group] }

func vocabKeys() string {
	keys := make([]string, 0, len(vocab))
	for k := range vocab {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// Scoped — метка группы из списка, пусто, если группы нет.
func Scoped(labels []string, prefix string) string {
	for _, l := range labels {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

// ScopedValue — значение scoped-метки без префикса группы.
func ScopedValue(labels []string, prefix string) string {
	return strings.TrimPrefix(Scoped(labels, prefix), prefix)
}

// WithStatus — метки после смены состояния: снять любую status:: и
// поставить одну. Идемпотентно: двух меток одной группы быть не должно,
// GitLab молча оставит одну, и какую — не скажет.
func WithStatus(existing []string, status string) ([]string, error) {
	allowed := append(append([]string{}, openStatuses...), closeStatuses...)
	if !has(allowed, status) {
		return nil, fmt.Errorf("unknown status '%s'; one of: %s", status, strings.Join(allowed, ", "))
	}
	out := make([]string, 0, len(existing)+1)
	for _, l := range existing {
		if !strings.HasPrefix(l, "status::") {
			out = append(out, l)
		}
	}
	return append(out, "status::"+status), nil
}

// RelabelAction — чем меняет задачу смена статуса: reopen либо relabel.
// Открытый статус на закрытой задаче открывает её — иначе задача с меткой
// очереди молча сидит среди закрытых и пропадает из списка. Закрывать
// relabel не умеет: это close, с комментарием.
func RelabelAction(state, status string) string {
	if has(openStatuses, status) && state == "closed" {
		return "reopen"
	}
	return "relabel"
}

// Эпик — не сущность GitLab (она в платной редакции), а маркер первой
// строкой тела. Дёшево, видно человеку в вебе и не требует прав сверх
// обычной задачи.
var epicRe = regexp.MustCompile(`(?m)^\*\*Эпик:\*\*\s*#(\d+)\s*$`)

// EpicOf — родительский эпик задачи либо 0. Считается только первая
// строка: ссылка на эпик где-то в теле — это ссылка, а не родство.
func EpicOf(body string) int {
	first := ""
	if rest := strings.TrimLeft(body, " \t\n"); rest != "" {
		first = strings.SplitN(rest, "\n", 2)[0]
	}
	m := epicRe.FindStringSubmatch(first)
	if m == nil {
		return 0
	}
	var n int
	fmt.Sscanf(m[1], "%d", &n)
	return n
}

// WithEpic — тело задачи с маркером эпика первой строкой. Идемпотентно,
// и старый маркер заменяется: два маркера означали бы двух родителей.
func WithEpic(body string, iid int) string {
	body = strings.TrimLeft(body, " \t\n")
	if EpicOf(body) != 0 {
		lines := strings.SplitN(body, "\n", 2)
		if len(lines) == 2 {
			body = strings.TrimLeft(lines[1], "\n")
		} else {
			body = ""
		}
	}
	marker := fmt.Sprintf("**Эпик:** #%d", iid)
	if body == "" {
		return marker
	}
	return marker + "\n\n" + body
}

// BareBody — тело без маркера эпика первой строкой.
func BareBody(body string) string {
	if EpicOf(body) == 0 {
		return body
	}
	lines := strings.SplitN(body, "\n", 2)
	if len(lines) == 2 {
		return strings.TrimLeft(lines[1], "\n")
	}
	return ""
}

// EpicDecision — что сделать с эпиком при правке тела. Sentinel, а не
// ноль: ноль значит «снять».
type EpicDecision int

const (
	EpicKeep   EpicDecision = iota // оставить маркер нового тела или старого родителя
	EpicSet                        // поставить другой
	EpicRemove                     // снять
)

// ReplaceBody — новое тело вместо старого с учётом родства. Маркер эпика
// живёт первой строкой тела, и правка тела целиком молча уводила бы задачу
// из-под эпика. Новое тело со своим маркером — как есть; без него — под
// старым родителем, если decision не сказал иначе.
func ReplaceBody(old, newBody string, decision EpicDecision, epic int) string {
	newBody = strings.TrimSpace(newBody)
	switch decision {
	case EpicRemove:
		return BareBody(newBody) // снять: и тот маркер, что принесло новое тело
	case EpicSet:
		return WithEpic(newBody, epic)
	}
	if EpicOf(newBody) != 0 {
		return newBody
	}
	if parent := EpicOf(old); parent != 0 {
		return WithEpic(newBody, parent)
	}
	return newBody
}

// slugify — ASCII-слаг для имени ветки: не-ASCII выбрасывается, не
// транслитерируется — заголовки русские, а имена веток остаются
// английскими.
func slugify(s string) string {
	var b strings.Builder
	lastDash := true // ведущие дефисы не пишутся
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")[:min(40, b.Len())]
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// BranchName — имя ветки задачи: тип по меткам, номер, слаг.
func BranchName(labels []string, iid string, slug string) string {
	// Дефект перевешивает: задача не должна нести и bug, и тип, но если
	// несёт — правда о работе в дефекте.
	prefix := "feat"
	if has(labels, "bug") || has(labels, "type::bug") {
		prefix = "bug"
	} else {
		for kind, p := range branchTypes {
			if has(labels, kind) || has(labels, "type::"+kind) {
				prefix = p
				break
			}
		}
	}
	name := prefix + "/" + iid
	if slug != "" {
		if clean := slugify(slug); clean != "" {
			name += "-" + clean
		}
	}
	return name
}

// SortIssues — тяжёлые первыми: сначала sev, потом номер.
func SortIssues(issues []Issue) {
	sort.SliceStable(issues, func(a, b int) bool {
		ka := sevOrder[Scoped(issues[a].Labels(), "sev::")]
		kb := sevOrder[Scoped(issues[b].Labels(), "sev::")]
		if ka != kb {
			return ka < kb
		}
		return issues[a].IID() < issues[b].IID()
	})
}

// ChildrenOf — задачи эпика: те, у кого в первой строке тела стоит его
// маркер. Выборкой по телу, а не по связям GitLab: связи — платная
// редакция, а маркер виден и в вебе, и в выводе show.
func ChildrenOf(issues []Issue, epicIID int) []Issue {
	out := []Issue{}
	for _, i := range issues {
		if EpicOf(i.Body()) == epicIID {
			out = append(out, i)
		}
	}
	return out
}
