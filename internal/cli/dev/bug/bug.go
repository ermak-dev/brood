// Командлеты трекера: brood dev bug <глагол>. Печать — здесь, данные — в
// internal/gitlab.
package bug

import (
	"fmt"
	"strconv"
	"strings"

	"git.ermak.dev/ermak/brood/internal/cli"
	"git.ermak.dev/ermak/brood/internal/gitlab"
	"git.ermak.dev/ermak/brood/internal/render"
)

const usage = `project tracker: GitLab issues, one issue per unit of work

  brood dev bug list [--all] [--label L] [-t TEXT]   open issues, worst first
  brood dev bug show <iid>                           full body
  brood dev bug new "<title>" [--label L ...]        body from --body, --body-file or stdin
  brood dev bug new "<title>" --epic 12              child of epic #12
  brood dev bug epic <iid>                           the epic's children and progress
  brood dev bug edit <iid> [--title T] [--body TEXT|--body-file F]  replace the title and/or body
  brood dev bug comment <iid> "<text>"               comment (or - for stdin)
  brood dev bug start <iid> [slug] [--no-branch]     claim: status::wip + branch
  brood dev bug close <iid> --comment "..."          close, default status::fixed
  brood dev bug relabel <iid> [--status S]           change labels; live, wip, parked reopen
  brood dev bug labels                               the label vocabulary

Coordinates come from the working copy's git origin; credentials are
GITLAB_TOKEN, a personal access token, in .env. Titles, bodies and comments
are in Russian; branch names and commits stay English.
`

// Main — диспетчер группы bug.
func Main(argv []string) {
	if len(argv) == 0 {
		cli.Usage(usage)
	}
	verb, rest := argv[0], argv[1:]
	switch verb {
	case "list":
		list(rest)
	case "show":
		show(rest)
	case "new":
		create(rest)
	case "epic":
		epic(rest)
	case "edit":
		edit(rest)
	case "comment":
		comment(rest)
	case "start":
		start(rest)
	case "close":
		closeIssue(rest)
	case "relabel":
		relabel(rest)
	case "labels":
		labels()
	default:
		cli.Usage(usage)
	}
}

func mustClient() *gitlab.Client {
	c, err := gitlab.New()
	if err != nil {
		cli.Fail("%s", err)
	}
	return c
}

func list(argv []string) {
	f := cli.ParseFlags(argv, []string{"-t", "--text"}, []string{"--label"}, []string{"--all"})
	state := "opened"
	if f.Bool("--all") {
		state = "all"
	}
	text := f.String("-t")
	if text == "" {
		text = f.String("--text")
	}
	checked := []string{}
	for _, l := range f.Repeated("--label") {
		v, err := gitlab.CheckLabel(l)
		if err != nil {
			cli.Fail("%s", err)
		}
		checked = append(checked, v)
	}
	issues, err := mustClient().Issues(state, checked, text)
	if err != nil {
		cli.Fail("%s", err)
	}
	if len(issues) == 0 {
		fmt.Println("no issues")
		return
	}
	rows := [][]string{}
	for _, i := range issues {
		rows = append(rows, []string{
			"#" + strconv.Itoa(i.IID()),
			orDash(gitlab.ScopedValue(i.Labels(), "status::")),
			orDash(gitlab.ScopedValue(i.Labels(), "sev::")),
			orDash(gitlab.ScopedValue(i.Labels(), "component::")),
			i.Title(),
		})
	}
	for _, line := range render.Table(rows) {
		fmt.Println(line)
	}
}

func show(argv []string) {
	f := cli.ParseFlags(argv, nil, nil, nil)
	if f.Pos(0) == "" {
		cli.Usage("brood dev bug show <iid>\n")
	}
	i, err := mustClient().Issue(f.Pos(0))
	if err != nil {
		cli.Fail("%s", err)
	}
	fmt.Printf("#%d %s\n", i.IID(), i.Title())
	fmt.Printf("%s · %s · %s\n\n", i.State(), orDash(strings.Join(i.Labels(), ", ")), i.WebURL())
	body := i.Body()
	if body == "" {
		body = "(empty)"
	}
	fmt.Println(body)
}

func create(argv []string) {
	f := cli.ParseFlags(argv, []string{"--body", "--body-file", "--epic"}, []string{"--label"}, nil)
	title := f.Pos(0)
	if title == "" {
		cli.Usage("brood dev bug new \"<title>\" [--label L ...] [--body TEXT|--body-file F] [--epic N]\n")
	}
	checked := []string{}
	for _, l := range f.Repeated("--label") {
		v, err := gitlab.CheckLabel(l)
		if err != nil {
			cli.Fail("%s", err)
		}
		checked = append(checked, v)
	}
	if gitlab.Scoped(checked, "status::") == "" {
		checked = append(checked, "status::live")
	}
	bodyText := cli.ReadBody(f.String("--body"), f.String("--body-file"))
	if bodyText == "" {
		cli.Usage("issue body must not be empty; use --body, --body-file or stdin\n")
	}
	if epic := f.String("--epic"); epic != "" {
		n, err := strconv.Atoi(epic)
		if err != nil {
			cli.Fail("--epic: %v", err)
		}
		bodyText = gitlab.WithEpic(bodyText, n)
	}
	i, err := mustClient().CreateIssue(title, bodyText, checked)
	if err != nil {
		cli.Fail("%s", err)
	}
	fmt.Printf("#%d %s\n", i.IID(), i.WebURL())
}

func epic(argv []string) {
	f := cli.ParseFlags(argv, nil, nil, nil)
	if f.Pos(0) == "" {
		cli.Usage("brood dev bug epic <iid>\n")
	}
	n, err := strconv.Atoi(f.Pos(0))
	if err != nil {
		cli.Fail("iid: %v", err)
	}
	all, err := mustClient().Issues("all", nil, "")
	if err != nil {
		cli.Fail("%s", err)
	}
	kids := gitlab.ChildrenOf(all, n)
	if len(kids) == 0 {
		fmt.Printf("#%d: no children yet\n", n)
		return
	}
	// Закрытых считаем по состоянию задачи, а не по метке: закрыть можно
	// и из веба, и тогда метка отстанет от правды.
	done, rows := 0, [][]string{}
	for _, i := range kids {
		state := gitlab.ScopedValue(i.Labels(), "status::")
		if i.State() == "closed" {
			done++
			state = "closed"
		}
		rows = append(rows, []string{"#" + strconv.Itoa(i.IID()), orDash(state), i.Title()})
	}
	for _, line := range render.Table(rows) {
		fmt.Println(line)
	}
	fmt.Printf("\n%d/%d closed\n", done, len(kids))
}

func edit(argv []string) {
	f := cli.ParseFlags(argv, []string{"--title", "--body", "--body-file", "--epic"}, nil, []string{"--no-epic"})
	iid := f.Pos(0)
	if iid == "" {
		cli.Usage("brood dev bug edit <iid> [--title T] [--body TEXT|--body-file F|-] [--epic N | --no-epic]\n")
	}
	if f.String("--epic") != "" && f.Bool("--no-epic") {
		cli.Usage("--epic and --no-epic are mutually exclusive\n")
	}
	c := mustClient()
	payload := gitlab.EditPayload{}
	if title := f.String("--title"); title != "" {
		payload["title"] = title
	}
	body, bodyFile := f.String("--body"), f.String("--body-file")
	epicSet, noEpic := f.String("--epic"), f.Bool("--no-epic")
	if body != "" || bodyFile != "" || epicSet != "" || noEpic {
		current, err := c.Issue(iid)
		if err != nil {
			cli.Fail("%s", err)
		}
		old := current.Body()
		decision, epicNum := gitlab.EpicKeep, 0
		switch {
		case noEpic:
			decision = gitlab.EpicRemove
		case epicSet != "":
			n, err := strconv.Atoi(epicSet)
			if err != nil {
				cli.Fail("--epic: %v", err)
			}
			decision, epicNum = gitlab.EpicSet, n
		}
		newBody := cli.ReadBody(body, bodyFile)
		if body == "" && bodyFile == "" {
			// Родство без нового тела: то же тело, другой (или никакой) маркер.
			newBody = old
		}
		payload["description"] = gitlab.ReplaceBody(old, newBody, decision, epicNum)
	}
	if err := c.Edit(iid, payload); err != nil {
		if strings.HasPrefix(err.Error(), "nothing to edit") {
			cli.Fail("%s", err)
		}
		cli.Fail("%s", err)
	}
	fmt.Printf("#%s edited\n", iid)
}

func comment(argv []string) {
	f := cli.ParseFlags(argv, []string{"--body-file"}, nil, nil)
	if f.Pos(0) == "" {
		cli.Usage("brood dev bug comment <iid> \"<text>\" | - | --body-file F\n")
	}
	if err := mustClient().Comment(f.Pos(0), cli.ReadBody(f.Pos(1), f.String("--body-file"))); err != nil {
		cli.Fail("%s", err)
	}
	fmt.Printf("#%s commented\n", f.Pos(0))
}

func start(argv []string) {
	f := cli.ParseFlags(argv, []string{"--worker"}, nil, []string{"--no-branch"})
	iid := f.Pos(0)
	if iid == "" {
		cli.Usage("brood dev bug start <iid> [slug] [--no-branch] [--worker W]\n")
	}
	c := mustClient()
	i, err := c.Issue(iid)
	if err != nil {
		cli.Fail("%s", err)
	}
	// Слаг по умолчанию — заголовок задачи: не-ASCII выбрасывается слагом,
	// русские заголовки дают короткие английские имена веток.
	name := f.Pos(1)
	if name == "" {
		name = i.Title()
	}
	branch := gitlab.BranchName(i.Labels(), iid, name)
	labels, err := gitlab.WithStatus(i.Labels(), "wip")
	if err != nil {
		cli.Fail("%s", err)
	}
	if err := c.Relabel(iid, labels); err != nil {
		cli.Fail("%s", err)
	}
	note := fmt.Sprintf("Работа начата в ветке `%s`", branch)
	if worker := f.String("--worker"); worker != "" {
		note += fmt.Sprintf(" · воркер %s", worker)
	}
	if err := c.Comment(iid, note+"."); err != nil {
		cli.Fail("%s", err)
	}
	if f.Bool("--no-branch") {
		// Ветку заводит исполнитель в своём клоне, поэтому у мастера есть
		// --no-branch: общий чекаут мастера не переключается никогда.
		fmt.Printf("#%s claimed; branch for the executor: %s\n", iid, branch)
		return
	}
	cli.Git("fetch", "origin")
	cli.Git("checkout", "-b", branch, cli.DefaultBranch())
	fmt.Printf("#%s claimed, on %s\n", iid, branch)
}

func closeIssue(argv []string) {
	f := cli.ParseFlags(argv, []string{"--comment", "--status"}, nil, nil)
	iid := f.Pos(0)
	if iid == "" {
		cli.Usage("brood dev bug close <iid> [--comment \"...\"] [--status S]\n")
	}
	status := f.String("--status")
	if status == "" {
		status = "fixed"
	}
	if !contains(gitlab.CloseStatuses(), status) {
		cli.Fail("unknown close status '%s'; one of: %s", status, strings.Join(gitlab.CloseStatuses(), ", "))
	}
	c := mustClient()
	i, err := c.Issue(iid)
	if err != nil {
		cli.Fail("%s", err)
	}
	if comment_ := f.String("--comment"); comment_ != "" {
		if err := c.Comment(iid, comment_); err != nil {
			cli.Fail("%s", err)
		}
	}
	labels, err := gitlab.WithStatus(i.Labels(), status)
	if err != nil {
		cli.Fail("%s", err)
	}
	if err := c.Close(iid, labels); err != nil {
		cli.Fail("%s", err)
	}
	fmt.Printf("#%s closed as %s\n", iid, status)
}

func relabel(argv []string) {
	f := cli.ParseFlags(argv, []string{"--status"}, []string{"--label"}, nil)
	iid := f.Pos(0)
	if iid == "" {
		cli.Usage("brood dev bug relabel <iid> [--status S] [--label L ...]\n")
	}
	status := f.String("--status")
	if status != "" && !contains(append(gitlab.OpenStatuses(), gitlab.CloseStatuses()...), status) {
		cli.Fail("unknown status '%s'; one of: %s", status,
			strings.Join(append(gitlab.OpenStatuses(), gitlab.CloseStatuses()...), ", "))
	}
	c := mustClient()
	i, err := c.Issue(iid)
	if err != nil {
		cli.Fail("%s", err)
	}
	next := i.Labels()
	if status != "" {
		next, err = gitlab.WithStatus(next, status)
		if err != nil {
			cli.Fail("%s", err)
		}
	}
	for _, l := range f.Repeated("--label") {
		v, err := gitlab.CheckLabel(l)
		if err != nil {
			cli.Fail("%s", err)
		}
		group, _, _ := strings.Cut(v, "::")
		filtered := next[:0]
		for _, x := range next {
			if !strings.HasPrefix(x, group+"::") {
				filtered = append(filtered, x)
			}
		}
		next = append(filtered, v)
	}
	// Открытый статус на закрытой задаче открывает её — иначе задача с
	// меткой очереди молча сидит среди закрытых.
	if gitlab.RelabelAction(i.State(), status) == "reopen" {
		err = c.Reopen(iid, next)
	} else {
		err = c.Relabel(iid, next)
	}
	if err != nil {
		cli.Fail("%s", err)
	}
	fmt.Printf("#%s: %s\n", iid, strings.Join(next, ", "))
}

func labels() {
	rows := [][]string{}
	for _, group := range []string{"status", "sev", "component", "type"} {
		rows = append(rows, []string{group + "::", strings.Join(gitlab.Vocab(group), ", ")})
	}
	for _, line := range render.Table(rows) {
		fmt.Println(line)
	}
	fmt.Println("\nOne label from each group per issue: a second one of the same group silently replaces the first.")
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
