// bro build declares the project and asks the server to build its agent image.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"brood.dev/go/brood/build"
	"brood.dev/go/brood/clientapi"
	"brood.dev/go/brood/gitcredential"
	"brood.dev/go/brood/projectsecret"
)

// Сервер держит ограниченный хвост лога: опрос чаще не даёт новых данных.
const imagePollInterval = 3 * time.Second

// Столько опросов подряд может не удаться, прежде чем ожидание сдаётся.
const imagePollFailures = 5

func buildUsage(w io.Writer) {
	fmt.Fprint(w, `usage: bro build [--pool NAME] [--ref COMMIT] [--no-wait] [--no-secrets]
       bro build --plan

Read ./brood.yaml, the Ansible playbook in the repository root. Exactly one of
its plays declares the project in the reserved variable vars.brood:

  vars:
    brood:
      project: my-project          required
      description: TEXT            optional
      repository: URL              optional; default: Git origin as HTTPS; "" disables cloning
      git_credential: NAME         optional server-side Git credential
      limits:                      optional; integer or null (unlimited), 0 forbids;
        swarms: 4                  a present block sets all four limits
        agents: 12
        cpu_millicores: 16000
        memory_bytes: 34359738368
      secrets:                     optional; values never appear in brood.yaml
        env: .env                  local dotenv file
        files:
          - path: .config/gh/token   destination in the project checkout
            source: secrets/gh-token local file

Everything under brood must be literal: bro reads plain YAML and does not
evaluate templates, so {{ }}, {% %} and YAML tags are refused there. The rest
of the playbook is not validated by bro.

bro build synchronizes the project with the logged-in server, makes the
project secrets on the server equal to brood.secrets, then asks the server to
build the agent image. The server clones the project repository at the commit
and runs brood.yaml with Ansible inside a fresh container; agents are clones
of the result. Nothing is built locally and uncommitted or unpushed changes
are not part of the image. When no play has tasks, roles, pre_tasks,
post_tasks, handlers or import_playbook, only the project and its secrets are
synchronized.

Secrets are delivered to agents at start, never to the image build. Sources
are relative to the repository root, must stay inside it, and are read from
the working tree, not from the commit. The dotenv file holds KEY=VALUE lines,
# comments and blank lines; a leading "export " and single or double quotes
around the value are accepted. Before writing, the names being set and removed
are printed to stderr; values are never printed. Without a secrets block the
project secrets are left untouched; an empty block (secrets: {}) removes them
all. Synchronizing secrets requires an administrator.

options:
  --pool NAME    build in this pool (default: chosen by the server)
  --ref COMMIT   full 40-character commit to build (default: HEAD of this checkout)
  --no-wait      print the image ID and return without waiting for the build
  --no-secrets   do not read or synchronize the declared secrets
  --plan         print the resolved declaration as JSON with secret names only;
                 offline, no synchronization

While waiting, the build log is printed as it arrives. Ctrl-C stops waiting
but does not cancel the build. The exit status is 0 only for a ready image.

example: bro build --pool brood
`)
}

func (c command) build(ctx context.Context, args []string, guest bool) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	pool := flags.String("pool", "", "pool")
	ref := flags.String("ref", "", "commit")
	noWait := flags.Bool("no-wait", false, "do not wait")
	noSecrets := flags.Bool("no-secrets", false, "skip secrets")
	planOnly := flags.Bool("plan", false, "print plan")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			buildUsage(c.out)
			return nil
		}
		return err
	}
	if flags.NArg() > 0 {
		return errors.New("unexpected build arguments")
	}
	if *ref != "" && !build.ValidCommit(*ref) {
		return errors.New("--ref requires a full 40-character lowercase hexadecimal commit")
	}
	project, err := build.Load(".")
	if err != nil {
		return err
	}
	if *planOnly {
		plan, err := project.Plan()
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(c.out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(plan)
	}
	if guest {
		return errors.New("project synchronization requires operator mode; use --plan in guest mode")
	}
	if !project.RepositoryResolved {
		return errors.New("project repository is unresolved; configure Git origin or set brood.repository in " + build.Filename + " (an explicit empty string disables cloning)")
	}
	// Commit и секреты читаются до первого запроса: отказ не оставляет полдела.
	commit := *ref
	if project.HasTasks && commit == "" {
		if commit, err = build.Head(project.Root); err != nil {
			return fmt.Errorf("%w; commit the project or pass --ref COMMIT", err)
		}
	}
	syncSecrets := project.Secrets != nil && !*noSecrets
	var secretFiles []projectsecret.File
	var secretVars map[string]string
	if syncSecrets {
		if secretFiles, secretVars, err = project.ReadSecrets(); err != nil {
			return err
		}
	}
	client, sessions, err := newClient("", "")
	if err != nil {
		return err
	}
	if client.Stored().Session == "" {
		return errNotLoggedIn
	}
	synchronized, err := client.ProjectSync(ctx, project.Config)
	if err != nil {
		return fmt.Errorf("project synchronization: %w", err)
	}
	reference := synchronized.GitCredential
	switch {
	case reference != "" && !synchronized.GitCredentialBound && gitcredential.ValidateName(reference) == nil:
		fmt.Fprintf(c.diagnostic, "project synchronized; Git credential %s is not bound to its repository; run: bro secret --project %s git add %s --repository URL --username USER --stdin\n", reference, project.Name, reference)
	case synchronized.Repository != "" && synchronized.OriginMode == "":
		fmt.Fprintln(c.diagnostic, "project synchronized; no Git access mode is configured for its repository")
	}
	if err := sessions.Save(client.Origin(), client.Stored()); err != nil {
		return err
	}
	switch {
	case syncSecrets:
		err := c.syncSecrets(ctx, client, synchronized.ID, secretFiles, secretVars)
		// Сессию сохраняем и после отказа: часть запросов могла пройти.
		if saveErr := sessions.Save(client.Origin(), client.Stored()); err == nil {
			err = saveErr
		}
		if err != nil {
			return err
		}
	case project.Secrets != nil:
		fmt.Fprintln(c.diagnostic, "project secrets: skipped (--no-secrets)")
	}
	if !project.HasTasks {
		fmt.Fprintf(c.out, "project %s synchronized; no image tasks are declared in %s\n", project.Name, build.Filename)
		return nil
	}
	if synchronized.Repository == "" {
		return errors.New("project has no repository; the server cannot clone it to build an image")
	}
	if *ref == "" {
		if dirty, known := build.Dirty(project.Root); known && dirty {
			fmt.Fprintln(c.diagnostic, "warning: uncommitted changes are not part of the image; the server builds the commit")
		}
	}
	if published, known := build.Published(project.Root, commit); known && !published {
		fmt.Fprintf(c.diagnostic, "warning: commit %s is not in any remote-tracking branch; the server clones %s and cannot see unpushed commits\n", commit, synchronized.Repository)
	}
	image, err := client.ImageBuild(ctx, synchronized.ID, clientapi.ImageBuildRequest{Ref: commit, Playbook: build.Filename, Pool: *pool})
	if err != nil {
		return fmt.Errorf("image build request: %w", err)
	}
	if *noWait {
		fmt.Fprintln(c.out, image.ID)
		return nil
	}
	fmt.Fprintf(c.diagnostic, "image %s: building commit %s with %s\n", image.ID, commit, build.Filename)
	return c.waitImage(ctx, client, synchronized.ID, image)
}

// syncSecrets приводит секреты проекта на сервере к декларации. API секретов
// не заменяет набор одним вызовом: лишнее удаляется, объявленное пишется
// заново, потому что значения с сервера не читаются и сравнить их нельзя.
// Удаление идёт первым, чтобы запись не упёрлась в лимиты и конфликты путей.
// На stderr уходят только имена.
func (c command) syncSecrets(ctx context.Context, client *clientapi.Client, project string, files []projectsecret.File, vars map[string]string) error {
	refusal := func(step string, err error) error {
		if errors.Is(err, clientapi.ErrSessionExpired) {
			return fmt.Errorf("project secrets: %s: %w", step, err)
		}
		return fmt.Errorf("project secrets: %s: %w; project secrets require an administrator (skip with --no-secrets)", step, err)
	}
	current, err := client.ProjectSecrets(ctx, project)
	if err != nil {
		return refusal("list", err)
	}
	setVars := make([]string, 0, len(vars))
	for name := range vars {
		setVars = append(setVars, name)
	}
	sort.Strings(setVars)
	setFiles := make([]string, 0, len(files))
	declaredFiles := map[string]bool{}
	for _, file := range files {
		setFiles = append(setFiles, file.Name)
		declaredFiles[file.Name] = true
	}
	var removeVars, removeFiles []string
	for _, name := range current.Vars {
		if _, declared := vars[name]; !declared {
			removeVars = append(removeVars, name)
		}
	}
	for _, file := range current.Files {
		if !declaredFiles[file.Name] {
			removeFiles = append(removeFiles, file.Name)
		}
	}
	sort.Strings(removeVars)
	sort.Strings(removeFiles)
	if len(setVars)+len(setFiles)+len(removeVars)+len(removeFiles) == 0 {
		fmt.Fprintln(c.diagnostic, "project secrets: none declared, none stored; nothing to do")
		return nil
	}
	for _, change := range []struct {
		label string
		names []string
	}{
		{"remove variables", removeVars},
		{"remove files", removeFiles},
		{"set variables", setVars},
		{"set files", setFiles},
	} {
		if len(change.names) > 0 {
			fmt.Fprintf(c.diagnostic, "project secrets: %s: %s\n", change.label, strings.Join(change.names, ", "))
		}
	}
	if len(removeVars) > 0 {
		if _, err := client.DeleteProjectSecretVars(ctx, project, removeVars); err != nil {
			return refusal("remove variables", err)
		}
	}
	if len(removeFiles) > 0 {
		if _, err := client.DeleteProjectSecretFiles(ctx, project, removeFiles); err != nil {
			return refusal("remove files", err)
		}
	}
	if len(vars) > 0 {
		if _, err := client.PutProjectSecretVars(ctx, project, vars); err != nil {
			return refusal("set variables", err)
		}
	}
	if len(files) > 0 {
		if _, err := client.PutProjectSecretFiles(ctx, project, files); err != nil {
			return refusal("set files", err)
		}
	}
	return nil
}

// waitImage печатает прирост лога и ждёт конечного состояния образа.
func (c command) waitImage(ctx context.Context, client *clientapi.Client, project string, image clientapi.Image) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	ticker := time.NewTicker(imagePollInterval)
	defer ticker.Stop()
	printed, failures := "", 0
	for {
		printed = c.printLog(printed, image.Log)
		switch image.State {
		case clientapi.ImageReady:
			fmt.Fprintf(c.out, "image %s ready\n", image.ID)
			return nil
		case clientapi.ImageFailed:
			reason := image.Error
			if reason == "" {
				reason = "the server gave no reason"
			}
			return fmt.Errorf("image %s failed: %s", image.ID, reason)
		case clientapi.ImageBuilding:
		default:
			return fmt.Errorf("image %s is %s, not ready", image.ID, image.State)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped waiting; the build of image %s continues on the server (see: bro image list)", image.ID)
		case <-ticker.C:
		}
		current, err := client.Image(ctx, project, image.ID)
		switch {
		case err == nil:
			image, failures = current, 0
		case ctx.Err() != nil:
			// Прерывание посреди запроса: отчёт даст следующий оборот цикла.
		case errors.Is(err, clientapi.ErrSessionExpired):
			return err
		default:
			if failures++; failures >= imagePollFailures {
				return fmt.Errorf("lost track of image %s, its build may continue on the server: %w", image.ID, err)
			}
		}
	}
}

// printLog печатает ещё не показанную часть лога и возвращает показанное.
// Сервер хранит хвост: когда прежний текст уже не префикс, хвост печатается
// целиком после строки «...».
func (c command) printLog(printed, log string) string {
	switch {
	case log == printed:
		return printed
	case strings.HasPrefix(log, printed):
		fmt.Fprint(c.diagnostic, log[len(printed):])
	default:
		if printed != "" && !strings.HasSuffix(printed, "\n") {
			fmt.Fprintln(c.diagnostic)
		}
		fmt.Fprint(c.diagnostic, "...\n", log)
	}
	return log
}
