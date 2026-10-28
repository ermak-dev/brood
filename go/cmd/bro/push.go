package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"brood.dev/go/brood/build"
	"brood.dev/go/brood/clientapi"
	"brood.dev/go/brood/oci"
)

func pushUsage(w io.Writer) {
	fmt.Fprint(w, `usage: bro push [target] [options]

Upload the completed .brood/build/<target> bundle to the logged-in server.
No Docker command or rebuild is performed. Server TLS verification is required.

options:
  --project NAME   destination project ID/name (or BUILD_PROJECT/build metadata)
  --ca FILE        additional trusted server CA (normally saved by bro login)

A server project with one accessible swarm can be selected by its ID/name.
If it contains several swarms, select the intended swarm by ID/name.
Without a target, use defaultTarget from ./Broodfile.
The registry stays internal; uploads use the authenticated HTTPS session API.
No actors are created or started.
`)
}

func (c command) push(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("push", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	project := flags.String("project", os.Getenv("BUILD_PROJECT"), "destination project")
	ca := flags.String("ca", "", "additional trusted CA")
	var options, targets []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			targets = append(targets, arg)
			continue
		}
		options = append(options, arg)
		name, _, value := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		if !value && flags.Lookup(name) != nil {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return fmt.Errorf("--%s requires a value", name)
			}
			i++
			options = append(options, args[i])
		}
	}
	if err := flags.Parse(options); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			pushUsage(c.out)
			return nil
		}
		return err
	}
	if len(targets) > 1 || len(flags.Args()) > 0 {
		return errors.New("push accepts at most one target")
	}
	client, store, err := newClient("", *ca)
	if err != nil {
		return err
	}
	if client.Stored().Session == "" {
		return errors.New("not logged in; run bro login <server>")
	}
	if _, err := client.Whoami(ctx); err != nil {
		return sessionError(err)
	}
	target := ""
	if len(targets) > 0 {
		target = targets[0]
	} else {
		data, err := readBundleFile("Broodfile")
		if err != nil {
			return errors.New("specify a built target or provide ./Broodfile")
		}
		doc, err := build.ParseDocument(string(data), "Broodfile")
		if err != nil {
			return err
		}
		target, _ = doc["defaultTarget"].(string)
		if target == "" {
			if list, ok := doc["targets"].(map[string]any); ok && len(list) == 1 {
				for name := range list {
					target = name
				}
			}
		}
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`).MatchString(target) {
		return errors.New("invalid or missing built target")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	run, err := filepath.EvalSymlinks(filepath.Join(root, ".brood", "build", target))
	if err != nil {
		return errors.New("build artifacts missing; run bro build first")
	}
	runs := filepath.Join(root, ".brood", "build", ".runs", target)
	if filepath.Dir(run) != runs || !regexp.MustCompile(`^run-[a-f0-9]{32}$`).MatchString(filepath.Base(run)) {
		return errors.New("refusing foreign build artifact directory")
	}
	data, err := readBundleFile(filepath.Join(run, "build.json"))
	if err != nil {
		return err
	}
	var metadata struct {
		Target      string `json:"target"`
		Environment string `json:"environment"`
		Project     string `json:"atespace"`
		Image       string `json:"image"`
		Digest      string `json:"imageDigest"`
	}
	if json.Unmarshal(data, &metadata) != nil || metadata.Target != target || metadata.Environment == "" || !strings.HasSuffix(metadata.Image, "@"+metadata.Digest) {
		return errors.New("invalid build metadata")
	}
	template, err := readBundleFile(filepath.Join(run, "actor-template.json"))
	if err != nil {
		return err
	}
	policy, err := readBundleFile(filepath.Join(run, "egress-policy.json"))
	if err != nil {
		return err
	}
	archive, err := oci.Open(filepath.Join(run, "image.oci.tar"), metadata.Digest)
	if err != nil {
		return err
	}
	defer archive.Close()
	selected := *project
	if selected == "" {
		selected = metadata.Project
	}
	swarm, err := resolvePushProject(ctx, client, selected)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	image, err := client.PushArchive(ctx, swarm.ID, metadata.Environment, archive)
	if err != nil {
		return sessionError(err)
	}
	template, err = rebindTemplate(template, metadata.Image, image)
	if err != nil {
		return err
	}
	result, err := client.PushTemplate(ctx, swarm.ID, metadata.Environment, image, template, policy)
	if err != nil {
		return sessionError(err)
	}
	if err := store.Save(client.Origin(), client.Stored()); err != nil {
		return err
	}
	fmt.Fprintln(c.out, result.Image)
	fmt.Fprintf(c.out, "template %s (project %s)\n", result.Template, result.Project)
	if result.EgressPolicyDeferred {
		fmt.Fprintln(c.out, "egress policy deferred: apply egress-policy.json when creating an actor; golden snapshots remain disabled")
	}
	return nil
}

func rebindTemplate(data []byte, original, image string) ([]byte, error) {
	var template map[string]json.RawMessage
	if json.Unmarshal(data, &template) != nil {
		return nil, errors.New("invalid actor template artifact")
	}
	var containers []map[string]json.RawMessage
	if json.Unmarshal(template["containers"], &containers) != nil || len(containers) == 0 {
		return nil, errors.New("actor template has no containers")
	}
	for _, container := range containers {
		var current string
		if json.Unmarshal(container["image"], &current) != nil || current != original {
			return nil, errors.New("actor template image does not match built image")
		}
		container["image"], _ = json.Marshal(image)
	}
	template["containers"], _ = json.Marshal(containers)
	return json.Marshal(template)
}

func readBundleFile(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("could not read build artifact")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4<<20 {
		return nil, errors.New("invalid or oversized build artifact")
	}
	data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
	if err != nil || len(data) > 4<<20 {
		return nil, errors.New("could not read build artifact")
	}
	return data, nil
}

func resolvePushProject(ctx context.Context, client *clientapi.Client, selection string) (clientapi.Swarm, error) {
	projects, err := client.ProjectsList(ctx)
	if err != nil {
		return clientapi.Swarm{}, sessionError(err)
	}
	var direct, group []clientapi.Swarm
	for _, project := range projects {
		swarms, err := client.SwarmsList(ctx, project.ID)
		if err != nil {
			return clientapi.Swarm{}, err
		}
		for _, swarm := range swarms {
			if swarm.ID == selection || swarm.Name == selection {
				direct = append(direct, swarm)
			}
			if project.ID == selection || project.Name == selection {
				group = append(group, swarm)
			}
		}
	}
	matches := direct
	if len(matches) == 0 {
		matches = group
	}
	if len(matches) == 0 {
		return clientapi.Swarm{}, errors.New("project not found or not accessible; create a project and personal swarm first")
	}
	if len(matches) != 1 {
		return clientapi.Swarm{}, errors.New("project is ambiguous; use the intended swarm ID with --project")
	}
	if matches[0].State != "active" {
		return clientapi.Swarm{}, errors.New("destination project is not active")
	}
	return matches[0], nil
}
