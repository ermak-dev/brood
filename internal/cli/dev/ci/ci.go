// Командлеты CI: brood dev ci <глагол>. Пайплайны, джобы, логи, линт,
// раннеры — GitLab CI из терминала.
package ci

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"git.ermak.dev/ermak/brood/internal/cli"
	"git.ermak.dev/ermak/brood/internal/gitlab"
	"git.ermak.dev/ermak/brood/internal/render"
)

const usage = `pipelines, job logs, lint and runners: GitLab CI from the terminal

  brood dev ci                           the latest 15 pipelines
  brood dev ci <id> [--failed]           a pipeline's jobs; --failed: only real failures
  brood dev ci why <id>                  why it failed: failing jobs with their log tails
  brood dev ci log <job> [--lines N] [--full]
                                         a job's log, the last 60 lines by default
  brood dev ci lint [file]               validate a .gitlab-ci.yml, the working copy's by default
  brood dev ci retry <job>               run a finished job again
  brood dev ci retry --pipeline <id>     rerun a pipeline's failed jobs
  brood dev ci cancel <id>               cancel a pipeline that is still going
  brood dev ci runners                   the project's runners and what each is running

Coordinates and credentials are those of ` + "`brood dev bug`" + `: the working
copy's git origin, GITLAB_TOKEN in .env.
`

// Main — диспетчер группы ci. Пусто — список, число — пайплайн: самое
// частое — самое короткое.
func Main(argv []string) {
	verb := "list"
	var rest []string
	switch {
	case len(argv) == 0:
	case isDigits(argv[0]):
		verb, rest = "show", argv
	default:
		verb, rest = argv[0], argv[1:]
	}
	switch verb {
	case "list":
		list(rest)
	case "show":
		show(rest)
	case "why":
		why(rest)
	case "log":
		log(rest)
	case "lint":
		lint(rest)
	case "retry":
		retry(rest)
	case "cancel":
		cancel(rest)
	case "runners":
		runners(rest)
	default:
		cli.Usage(usage)
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func mustClient() *gitlab.Client {
	c, err := gitlab.New()
	if err != nil {
		cli.Fail("%s", err)
	}
	return c
}

func list(argv []string) {
	f := cli.ParseFlags(argv, []string{"-n"}, nil, nil)
	got, err := mustClient().Pipelines(f.Int("-n", 15))
	if err != nil {
		cli.Fail("%s", err)
	}
	if len(got) == 0 {
		fmt.Println("no pipelines")
		return
	}
	rows := [][]string{}
	for _, p := range got {
		rows = append(rows, gitlab.PipelineLine(p))
	}
	for _, line := range render.Table(rows) {
		fmt.Println(line)
	}
}

func show(argv []string) {
	f := cli.ParseFlags(argv, nil, nil, []string{"--failed"})
	if f.Pos(0) == "" {
		cli.Usage("brood dev ci show <id> [--failed]\n")
	}
	pid, err := strconv.Atoi(f.Pos(0))
	if err != nil {
		cli.Fail("id: %v", err)
	}
	c := mustClient()
	pipe, err := c.PipelineByID(pid)
	if err != nil {
		cli.Fail("%s", err)
	}
	jobs, err := c.Jobs(pid)
	if err != nil {
		cli.Fail("%s", err)
	}
	if f.Bool("--failed") {
		filtered := jobs[:0]
		for _, j := range jobs {
			if gitlab.IsFailure(j) {
				filtered = append(filtered, j)
			}
		}
		jobs = filtered
	}
	for _, line := range render.Table([][]string{gitlab.PipelineLine(pipe)}) {
		fmt.Println(line)
	}
	if len(jobs) == 0 {
		if f.Bool("--failed") {
			fmt.Println("no failed jobs")
		} else {
			fmt.Println("no jobs")
		}
		return
	}
	fmt.Println()
	rows := [][]string{}
	for _, j := range jobs {
		rows = append(rows, gitlab.JobLine(j))
	}
	for _, line := range render.Table(rows) {
		fmt.Println(line)
	}
}

// why — проваливших пайплайн джоб с хвостами логов; хвост короче, чем у
// log: провалившихся джоб бывает несколько, а подробности одной — это log.
const whyLines = 30

func why(argv []string) {
	f := cli.ParseFlags(argv, nil, nil, nil)
	if f.Pos(0) == "" {
		cli.Usage("brood dev ci why <id>\n")
	}
	pid, err := strconv.Atoi(f.Pos(0))
	if err != nil {
		cli.Fail("id: %v", err)
	}
	c := mustClient()
	pipe, err := c.PipelineByID(pid)
	if err != nil {
		cli.Fail("%s", err)
	}
	jobs, err := c.Jobs(pid)
	if err != nil {
		cli.Fail("%s", err)
	}
	tails := map[int]string{}
	for _, j := range jobs {
		if gitlab.IsFailure(j) {
			trace, err := c.Trace(j.ID())
			if err != nil {
				cli.Fail("%s", err)
			}
			tails[j.ID()] = gitlab.Tail(cli.Plain(trace), whyLines)
		}
	}
	for _, line := range gitlab.WhyLines(pipe, jobs, tails) {
		fmt.Println(line)
	}
}

const defaultLogLines = 60

func log(argv []string) {
	f := cli.ParseFlags(argv, []string{"--lines"}, nil, []string{"--full"})
	if f.Pos(0) == "" {
		cli.Usage("brood dev ci log <job> [--lines N] [--full]\n")
	}
	jid, err := strconv.Atoi(f.Pos(0))
	if err != nil {
		cli.Fail("job: %v", err)
	}
	trace, err := mustClient().Trace(jid)
	if err != nil {
		cli.Fail("%s", err)
	}
	n := f.Int("--lines", defaultLogLines)
	if f.Bool("--full") {
		n = -1
	}
	text := gitlab.Tail(cli.Plain(trace), n)
	if text == "" {
		text = "(empty log)"
	}
	fmt.Println(text)
}

func lint(argv []string) {
	f := cli.ParseFlags(argv, nil, nil, nil)
	file := f.Pos(0)
	if file == "" {
		file = ".gitlab-ci.yml"
	}
	content, err := os.ReadFile(file)
	if err != nil {
		cli.Fail("cannot read %s: %v", file, err)
	}
	result, err := mustClient().Lint(string(content))
	if err != nil {
		cli.Fail("%s", err)
	}
	lines, valid := gitlab.LintReport(result)
	for _, line := range lines {
		fmt.Println(line)
	}
	if !valid {
		os.Exit(1)
	}
}

func retry(argv []string) {
	f := cli.ParseFlags(argv, []string{"--pipeline"}, nil, nil)
	c := mustClient()
	if pid := f.String("--pipeline"); pid != "" {
		id, err := strconv.Atoi(pid)
		if err != nil {
			cli.Fail("--pipeline: %v", err)
		}
		pipe, err := c.PipelineByID(id)
		if err != nil {
			cli.Fail("%s", err)
		}
		if refused := gitlab.PipelineRetryBlocked(pipe); refused != "" {
			cli.Fail("%s", refused)
		}
		got, err := c.RetryPipeline(id)
		if err != nil {
			cli.Fail("%s", err)
		}
		fmt.Printf("pipeline %d %s: %s\n", got.ID(), got.Status(), got.WebURL())
		return
	}
	if f.Pos(0) == "" {
		cli.Fail("a job id or --pipeline <id>, one of them")
	}
	jid, err := strconv.Atoi(f.Pos(0))
	if err != nil {
		cli.Fail("job: %v", err)
	}
	job, err := c.Job(jid)
	if err != nil {
		cli.Fail("%s", err)
	}
	if refused := gitlab.RetryJobBlocked(job); refused != "" {
		cli.Fail("%s", refused)
	}
	got, err := c.RetryJob(jid)
	if err != nil {
		cli.Fail("%s", err)
	}
	url, _ := got["web_url"].(string)
	fmt.Printf("job %d %s: %s\n", got.ID(), got.Status(), url)
}

func cancel(argv []string) {
	f := cli.ParseFlags(argv, nil, nil, nil)
	if f.Pos(0) == "" {
		cli.Usage("brood dev ci cancel <id>\n")
	}
	pid, err := strconv.Atoi(f.Pos(0))
	if err != nil {
		cli.Fail("id: %v", err)
	}
	c := mustClient()
	pipe, err := c.PipelineByID(pid)
	if err != nil {
		cli.Fail("%s", err)
	}
	// GitLab принял бы отмену законченного пайплайна и молча не сделал бы
	// ничего — отказ с именем состояния до запроса.
	if refused := gitlab.PipelineCancelBlocked(pipe); refused != "" {
		cli.Fail("%s", refused)
	}
	if err := c.CancelPipeline(pid); err != nil {
		cli.Fail("%s", err)
	}
}

func runners(argv []string) {
	if len(argv) > 0 {
		cli.Fail("brood dev ci runners takes no arguments")
	}
	c := mustClient()
	got, err := c.Runners()
	if err != nil {
		cli.Fail("%s", err)
	}
	if len(got) == 0 {
		fmt.Println("no runners")
	} else {
		rows := [][]string{}
		for _, r := range got {
			rows = append(rows, gitlab.RunnerLine(r))
		}
		for _, line := range render.Table(rows) {
			fmt.Println(line)
		}
	}
	running, err := c.RunningJobs()
	if err != nil {
		cli.Fail("%s", err)
	}
	fmt.Println()
	if len(running) == 0 {
		fmt.Println("no running jobs")
		return
	}
	rows := [][]string{}
	for _, j := range running {
		rows = append(rows, gitlab.RunningLine(j))
	}
	for _, line := range render.Table(rows) {
		fmt.Println(line)
	}
}

var _ = strings.TrimSpace
