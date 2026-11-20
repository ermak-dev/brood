// bro build declares the project and asks the server to build its worker image.
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
	"strings"
	"time"

	"brood.dev/go/brood/build"
	"brood.dev/go/brood/clientapi"
	"brood.dev/go/brood/gitcredential"
)

// Сервер держит ограниченный хвост лога: опрос чаще не даёт новых данных.
const imagePollInterval = 3 * time.Second

// Столько опросов подряд может не удаться, прежде чем ожидание сдаётся.
const imagePollFailures = 5

func buildUsage(w io.Writer) {
	fmt.Fprint(w, `usage: bro build [--pool NAME] [--ref COMMIT] [--no-wait]
       bro build --plan

Read ./Broodfile, synchronize the project with the logged-in server, then ask
the server to build the project's worker image. The server clones the project
repository at the commit and runs the Broodfile image.playbook with Ansible
inside a fresh container; workers are clones of the result. Nothing is built
locally and uncommitted or unpushed changes are not part of the image.
Without an image block in the Broodfile only the project is synchronized.

options:
  --pool NAME    build in this worker pool (default: chosen by the server)
  --ref COMMIT   full 40-character commit to build (default: HEAD of this checkout)
  --no-wait      print the image ID and return without waiting for the build
  --plan         print the resolved declaration as JSON; offline, no synchronization

While waiting, the build log is printed as it arrives. Ctrl-C stops waiting
but does not cancel the build. The exit status is 0 only for a ready image.
`)
}

func (c command) build(ctx context.Context, args []string, guest bool) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	pool := flags.String("pool", "", "worker pool")
	ref := flags.String("ref", "", "commit")
	noWait := flags.Bool("no-wait", false, "do not wait")
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
		encoder := json.NewEncoder(c.out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(project.Plan())
	}
	if guest {
		return errors.New("project synchronization requires operator mode; use --plan in guest mode")
	}
	if !project.RepositoryResolved {
		return errors.New("project repository is unresolved; configure Git origin or set project.repository in Broodfile (an explicit empty string disables cloning)")
	}
	// Commit разрешается до первого запроса: отказ не оставляет полдела.
	commit := *ref
	if project.Image != nil && commit == "" {
		if commit, err = build.Head(project.Root); err != nil {
			return fmt.Errorf("%w; commit the project or pass --ref COMMIT", err)
		}
	}
	client, sessions, err := newClient("", "")
	if err != nil {
		return err
	}
	if client.Stored().Session == "" {
		return errors.New("not logged in; run: bro login <server>")
	}
	synchronized, err := client.ProjectSync(ctx, project.Config)
	if err != nil {
		return fmt.Errorf("project synchronization: %w", sessionError(err))
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
	if project.Image == nil {
		fmt.Fprintf(c.out, "project %s synchronized; no image is declared in Broodfile\n", project.Name)
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
	image, err := client.ImageBuild(ctx, synchronized.ID, clientapi.ImageBuildRequest{Ref: commit, Playbook: project.Image.Playbook, Pool: *pool})
	if err != nil {
		return fmt.Errorf("image build request: %w", sessionError(err))
	}
	if *noWait {
		fmt.Fprintln(c.out, image.ID)
		return nil
	}
	fmt.Fprintf(c.diagnostic, "image %s: building commit %s with %s\n", image.ID, commit, project.Image.Playbook)
	return c.waitImage(ctx, client, synchronized.ID, image)
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
			return sessionError(err)
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
