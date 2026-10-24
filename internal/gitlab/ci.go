// Чистая логика CI: строки таблиц, вердикты пайплайна, отказы повторов.
// Метка — слово, а не значок: вывод читает модель, а у неё нет глаз,
// чтобы отличить зелёную галку от красного крестика. Провал — капсом:
// его и ищут в списке.
package gitlab

import (
	"fmt"
	"strings"
)

var marks = map[string]string{"success": "ok", "failed": "FAILED"}

// Ещё идущие: у GitLab это и created/pending/running, и промежуточные
// waiting_for_resource/preparing — ответ «подожди», а не «красный».
var pipelineWait = map[string]bool{
	"created": true, "waiting_for_resource": true, "preparing": true,
	"pending": true, "running": true,
}

// Законченные: отменять нечего. GitLab на отмену такого отвечает успехом
// и не делает ничего — молчаливое «сделано» хуже отказа.
var pipelineDone = map[string]bool{
	"success": true, "failed": true, "canceled": true, "skipped": true,
}

func mark(status string) string {
	if m, ok := marks[status]; ok {
		return m
	}
	if status == "" {
		return "?"
	}
	return status
}

// Duration — секунды GitLab -> "-" / "Ns" / "NmSSs". nil — джоба не
// стартовала, и ноль там был бы враньём.
func Duration(secs *float64) string {
	if secs == nil {
		return "-"
	}
	s := int(*secs)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}

// Tail — последние n строк; n < 0 — всё. Перевод строки в конце трассы
// строкой не считается: иначе «последние 60» были бы 59.
func Tail(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if n < 0 || n > len(lines) {
		n = len(lines)
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// ScreenLines — трасса как её видит терминал: от строки — то, что после
// последнего \r. GitLab пишет секции как «section_start:<t>:<имя>\r\x1b[0K<текст>»,
// прогресс — через \r; без этого хвост лога — мусор маркеров. \r\n — просто
// конец строки. Цвета остаются: их снимает тот, кто печатает.
func ScreenLines(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range lines {
		if idx := strings.LastIndexByte(line, '\r'); idx >= 0 {
			lines[i] = line[idx+1:]
		}
	}
	return strings.Join(lines, "\n")
}

// IsFailure — провал, который валит пайплайн: failed без allow_failure.
// Терпимый провал цвет не решает, и «почему красный» не должен звать его
// причиной.
func IsFailure(j Job) bool {
	return j.Status() == "failed" && !allowFailure(j)
}

func allowFailure(j Job) bool {
	b, _ := j["allow_failure"].(bool)
	return b
}

func waitRefusal(kind string, obj map[string]any) string {
	status, _ := obj["status"].(string)
	if pipelineWait[status] {
		return fmt.Sprintf("%s %v is %s: wait for it to finish before retrying", kind, obj["id"], status)
	}
	return ""
}

// RetryJobBlocked — отказ повтора идущей джобы либо "". GitLab ответил бы
// 403 без объяснения, и искать пришлось бы по коду ответа.
func RetryJobBlocked(j Job) string { return waitRefusal("job", j) }

// PipelineRetryBlocked — отказ повтора идущего пайплайна либо "".
func PipelineRetryBlocked(p Pipeline) string { return waitRefusal("pipeline", p) }

// PipelineCancelBlocked — отказ отмены законченного пайплайна либо "":
// GitLab принял бы её молча и ничего не сделал.
func PipelineCancelBlocked(p Pipeline) string {
	if pipelineDone[p.Status()] {
		return fmt.Sprintf("pipeline %d is %s: nothing to cancel", p.ID(), p.Status())
	}
	return ""
}

// PipelineLine — строка пайплайна для таблицы.
func PipelineLine(p Pipeline) []string {
	sha, _ := p["sha"].(string)
	if len(sha) > 8 {
		sha = sha[:8]
	}
	if sha == "" {
		sha = "-"
	}
	ref, _ := p["ref"].(string)
	created, _ := p["created_at"].(string)
	url := p.WebURL()
	if url == "" {
		url = "-"
	}
	return []string{mark(p.Status()), fmt.Sprint(p.ID()), sha, orDash(ref), when(created), url}
}

// JobLine — строка джобы. Терпимый провал — не капсом: пайплайн он не валил.
func JobLine(j Job) []string {
	allowed := ""
	if allowFailure(j) {
		allowed = "(allow_failure)"
	}
	return []string{mark(j.Status()), fmt.Sprint(j.ID()), orDash(j.Stage()), orDash(j.Name()),
		Duration(durationOf(j)), allowed}
}

// RunnerLine — ON/OFF — принимает ли раннер работу; status — видит ли его
// GitLab. Разные вещи: остановленный раннер бывает online, и джобы тогда
// стоят.
func RunnerLine(r Runner) []string {
	active, _ := r["active"].(bool)
	paused, _ := r["paused"].(bool)
	on := "OFF"
	if active && !paused {
		on = "ON"
	}
	desc, _ := r["description"].(string)
	status, _ := r["status"].(string)
	tags := strings.Join(StringSlice(r["tag_list"]), ",")
	return []string{on, fmt.Sprint(r.ID()), orDash(desc), orDash(status), orDash(tags)}
}

// RunningLine — строка идущей джобы с её раннером.
func RunningLine(j Job) []string {
	runner, _ := j["runner"].(map[string]any)
	held := "no runner"
	if runner != nil && runner["id"] != nil {
		desc, _ := runner["description"].(string)
		held = strings.TrimSpace(fmt.Sprintf("runner %v %s", runner["id"], desc))
	}
	ref, _ := j["ref"].(string)
	return []string{fmt.Sprint(j.ID()), orDash(j.Stage()), orDash(j.Name()), orDash(ref), held}
}

func durationOf(j Job) *float64 {
	v, ok := j["duration"].(float64)
	if !ok {
		return nil
	}
	return &v
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// when — 2026-09-24T10:11:12.345Z -> 2026-09-24 10:11 (UTC, как отдаёт GitLab).
func when(stamp string) string {
	if stamp == "" {
		return "-"
	}
	if len(stamp) >= 16 {
		return stamp[:16][:10] + " " + stamp[11:16]
	}
	return stamp
}

// PipelineFailure — причина провала от самого пайплайна -> строки.
// Отвергнутый .gitlab-ci.yml валит пайплайн без единой джобы: смотреть на
// джобы тогда бесполезно, причина лежит в пайплайне.
func PipelineFailure(p Pipeline) []string {
	out := []string{}
	if yaml, _ := p["yaml_errors"].(string); yaml != "" {
		out = append(out, "yaml_errors: "+yaml)
	}
	if reason, _ := p["failure_reason"].(string); reason != "" {
		out = append(out, "failure_reason: "+reason)
	}
	if detailed, ok := p["detailed_status"].(map[string]any); ok {
		if text, _ := detailed["text"].(string); text != "" {
			out = append(out, "detailed_status: "+text)
		}
	}
	return out
}

// WhyLines — почему пайплайн такой, какой есть. Терпимые провалы названы
// отдельно и прямо: иначе их красный цвет в вебе принимают за причину.
// Без провалившихся джоб — причина самого пайплайна.
func WhyLines(p Pipeline, jobs []Job, tails map[int]string) []string {
	status := p.Status()
	out := []string{fmt.Sprintf("pipeline %d %s: %s", p.ID(), status, p.WebURL())}
	var failed, tolerated []Job
	for _, j := range jobs {
		if IsFailure(j) {
			failed = append(failed, j)
		} else if j.Status() == "failed" && allowFailure(j) {
			tolerated = append(tolerated, j)
		}
	}
	for _, j := range failed {
		out = append(out, "", fmt.Sprintf("job %d %s/%s failed (%s): %s",
			j.ID(), j.Stage(), j.Name(), Duration(durationOf(j)), webURLOf(j)))
		if text, ok := tails[j.ID()]; ok && text != "" {
			out = append(out, strings.Split(text, "\n")...)
		} else {
			out = append(out, "(empty trace)")
		}
	}
	for _, j := range tolerated {
		out = append(out, "", fmt.Sprintf("job %d %s/%s failed (allow_failure): did NOT fail the pipeline",
			j.ID(), j.Stage(), j.Name()))
	}
	if len(failed) > 0 {
		return out
	}
	if status == "failed" {
		out = append(out, "no jobs failed; the pipeline itself says:")
		reasons := PipelineFailure(p)
		if len(reasons) == 0 {
			return append(out, "no jobs failed, and GitLab gives no reason")
		}
		return append(out, reasons...)
	}
	if len(tolerated) == 0 {
		out = append(out, "nothing failed")
	}
	return out
}

func webURLOf(j Job) string {
	s, _ := j["web_url"].(string)
	if s == "" {
		return "-"
	}
	return s
}

// LintReport — ответ /ci/lint -> (строки, годен ли).
func LintReport(result map[string]any) ([]string, bool) {
	valid, _ := result["valid"].(bool)
	var lines []string
	if valid {
		names := []string{}
		for _, j := range StringSlice2(result["jobs"]) {
			if m, ok := j.(map[string]any); ok {
				name, _ := m["name"].(string)
				if name == "" {
					name = "?"
				}
				names = append(names, name)
			}
		}
		lines = []string{fmt.Sprintf("valid — %d job(s): %s", len(names), strings.Join(names, ", "))}
	} else {
		lines = []string{"invalid"}
		for _, e := range StringSlice(result["errors"]) {
			lines = append(lines, "error: "+e)
		}
	}
	for _, w := range StringSlice(result["warnings"]) {
		lines = append(lines, "warning: "+w)
	}
	return lines, valid
}

// StringSlice2 — []any как есть (для jobs в lint).
func StringSlice2(v any) []any {
	s, _ := v.([]any)
	return s
}
