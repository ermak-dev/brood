package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"brood.dev/go/brood/clientapi"
	"brood.dev/go/brood/projectsecret"
)

func secretUsage(w io.Writer) {
	fmt.Fprint(w, `usage: bro secret --project ID_OR_NAME file add|list|remove [PATH...]
       bro secret --project ID_OR_NAME var add NAME --stdin
       bro secret --project ID_OR_NAME var add NAME --value-file FILE
       bro secret --project ID_OR_NAME var list|remove [NAME...]

Manage the secrets of a project. Project selection is always explicit. Values are never
accepted as arguments or printed. File sources must be regular non-symlink files
inside the current git working copy (512 KiB per file, 2 MiB per batch).
Lists show only file names and sizes or variable names. Mutations print nothing.

File and variable secrets are delivered to the project's running agents.
--ca FILE adds a trusted server CA; otherwise use the saved login trust.

A brood.secrets block in brood.yaml declares the complete set of file and var
secrets: bro build removes file and var secrets that the block does not list,
including ones added here by hand. Without that block bro build leaves them alone.

example: bro secret --project brood var add GH_TOKEN --stdin < token.txt
`)
}

type secretOptions struct {
	project, ca, kind, action, valueFile string
	stdin                                bool
	names                                []string
}

func parseSecret(args []string) (secretOptions, error) {
	var o secretOptions
	var positional []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--project", "--ca", "--value-file":
			if i+1 == len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
				return o, errors.New("secret option requires a value")
			}
			i++
			switch arg {
			case "--project":
				if o.project != "" {
					return o, errors.New("project must be specified once")
				}
				o.project = args[i]
			case "--ca":
				o.ca = args[i]
			case "--value-file":
				if o.valueFile != "" {
					return o, errors.New("value file must be specified once")
				}
				o.valueFile = args[i]
			}
		case "--stdin":
			if o.stdin {
				return o, errors.New("stdin must be specified once")
			}
			o.stdin = true
		default:
			if strings.HasPrefix(arg, "-") {
				return o, errors.New("unknown secret option; use bro secret --help")
			}
			positional = append(positional, arg)
		}
	}
	if o.project == "" {
		return o, errors.New("secret requires explicit --project ID_OR_NAME")
	}
	if len(positional) < 2 {
		return o, errors.New("secret requires file or var and add, list or remove")
	}
	o.kind, o.action, o.names = positional[0], positional[1], positional[2:]
	if o.kind != "file" && o.kind != "var" {
		return o, errors.New("secret kind must be file or var")
	}
	switch o.action {
	case "list":
		if len(o.names) != 0 {
			return o, errors.New("secret list accepts no names")
		}
	case "remove":
		if len(o.names) == 0 {
			return o, errors.New("secret remove requires names")
		}
	case "add":
		if len(o.names) == 0 {
			return o, errors.New("secret add requires names")
		}
	default:
		return o, errors.New("secret action must be add, list or remove")
	}
	if o.kind == "var" && o.action == "add" {
		if len(o.names) != 1 || o.stdin == (o.valueFile != "") {
			return o, errors.New(o.kind + " add requires one name and exactly one of --stdin or --value-file")
		}
	} else if o.stdin || o.valueFile != "" {
		return o, errors.New("value input options are only valid for var add")
	}
	for _, name := range o.names {
		var err error
		switch o.kind {
		case "var":
			err = projectsecret.ValidateVar(name, "")
		default:
			err = projectsecret.ValidateFileName(name)
		}
		if err != nil {
			return o, err
		}
	}
	return o, nil
}

func (c command) secret(ctx context.Context, args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		secretUsage(c.out)
		return nil
	}
	o, err := parseSecret(args)
	if err != nil {
		return err
	}
	var files []projectsecret.File
	var vars map[string]string
	if o.action == "add" {
		if o.kind == "file" {
			files, err = readSecretFiles(o.names)
		} else {
			var value string
			if o.stdin {
				value, err = readSecretValue(c.in)
			} else {
				value, err = readSecretInputFile(o.valueFile, readSecretValue)
			}
			if err == nil {
				err = projectsecret.ValidateVar(o.names[0], value)
			}
			vars = map[string]string{o.names[0]: value}
		}
		if err != nil {
			return err
		}
	}
	client, store, project, err := secretClient(ctx, o)
	if err != nil {
		return err
	}
	var metadata projectsecret.Metadata
	switch {
	case o.action == "list":
		metadata, err = client.ProjectSecrets(ctx, project)
	case o.kind == "file" && o.action == "add":
		metadata, err = client.PutProjectSecretFiles(ctx, project, files)
	case o.kind == "var" && o.action == "add":
		metadata, err = client.PutProjectSecretVars(ctx, project, vars)
	case o.kind == "file":
		metadata, err = client.DeleteProjectSecretFiles(ctx, project, o.names)
	default:
		metadata, err = client.DeleteProjectSecretVars(ctx, project, o.names)
	}
	if err != nil {
		return err
	}
	if err = store.Save(client.Origin(), client.Stored()); err != nil {
		return err
	}
	if o.action == "list" {
		if o.kind == "file" {
			for _, file := range metadata.Files {
				fmt.Fprintf(c.out, "%s\t%d\n", file.Name, file.Size)
			}
		} else {
			for _, name := range metadata.Vars {
				fmt.Fprintln(c.out, name)
			}
		}
	}
	return nil
}

// secretClient открывает сессию и разрешает явно выбранный проект.
func secretClient(ctx context.Context, o secretOptions) (*clientapi.Client, *clientapi.SessionStore, string, error) {
	client, store, err := newClient("", o.ca)
	if err != nil {
		return nil, nil, "", err
	}
	projects, err := client.ProjectsList(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	project, err := resolveProject(projects, o.project)
	if err != nil {
		return nil, nil, "", err
	}
	return client, store, project, nil
}

// resolveProject находит проект списка по точному ID или по точному имени;
// неоднозначное имя — отказ.
func resolveProject(projects []clientapi.Project, selected string) (string, error) {
	if selected == "" {
		return "", errors.New("explicit project is required")
	}
	for _, project := range projects {
		if project.ID == selected {
			return project.ID, nil
		}
	}
	var id string
	for _, project := range projects {
		if project.Name == selected {
			if id != "" {
				return "", errors.New("project name is ambiguous; use its ID")
			}
			id = project.ID
		}
	}
	if id == "" {
		return "", errors.New("project not found or not accessible")
	}
	return id, nil
}

// readSecretInput читает не больше limit байт значения (с запасом на
// перевод строки) и снимает не более одного завершающего LF или CRLF.
func readSecretInput(in io.Reader, limit int64) (string, bool) {
	data, err := io.ReadAll(io.LimitReader(in, limit+3))
	if err != nil {
		return "", false
	}
	value := strings.TrimSuffix(string(data), "\n")
	if len(data) > 0 && data[len(data)-1] == '\n' {
		value = strings.TrimSuffix(value, "\r")
	}
	return value, true
}

func readSecretValue(in io.Reader) (string, error) {
	value, ok := readSecretInput(in, projectsecret.MaxVarBytes)
	if !ok {
		return "", errors.New("could not read secret variable input")
	}
	// Вся проверка значений и лимитов остаётся в общем чистом пакете.
	if err := projectsecret.ValidateVar("SECRET_INPUT", value); err != nil {
		return "", err
	}
	return value, nil
}

// readSecretInputFile открывает обычный файл без следования симлинку и без
// блокировки на FIFO, затем читает значение тем же read.
func readSecretInputFile(name string, read func(io.Reader) (string, error)) (string, error) {
	file, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return "", errors.New("could not open secret input file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", errors.New("secret input must be a regular file")
	}
	return read(file)
}

func readSecretFiles(names []string) ([]projectsecret.File, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	data, err := cmd.Output()
	if err != nil {
		return nil, errors.New("secret file add requires a git working copy")
	}
	rootPath := strings.TrimSuffix(string(data), "\n")
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, errors.New("could not open git working copy")
	}
	defer root.Close()
	cwd, err := os.Getwd()
	if err != nil {
		return nil, errors.New("could not locate working directory")
	}
	var files []projectsecret.File
	total := 0
	for _, source := range names {
		if err := projectsecret.ValidateFileName(source); err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(rootPath, filepath.Join(cwd, filepath.FromSlash(source)))
		if err != nil {
			return nil, errors.New("secret source must reside inside the git working copy")
		}
		name := filepath.ToSlash(relative)
		if err := projectsecret.ValidateFileName(name); err != nil {
			return nil, err
		}
		content, err := readSecretRootFile(root, relative)
		if err != nil {
			return nil, err
		}
		total += len(content)
		if total > projectsecret.MaxTotalFileBytes {
			return nil, errors.New("secret files exceed total byte limit")
		}
		files = append(files, projectsecret.File{Name: name, Data: content})
	}
	if err := projectsecret.ValidateBatch(files, nil); err != nil {
		return nil, err
	}
	return files, nil
}

func readSecretRootFile(root *os.Root, name string) ([]byte, error) {
	// Root удерживает границу при переименовании каталогов; NOFOLLOW и NONBLOCK
	// не дают подменённому последнему компоненту стать ссылкой или зависшим FIFO.
	parts := strings.Split(filepath.ToSlash(name), "/")
	for i := range parts {
		info, err := root.Lstat(filepath.FromSlash(strings.Join(parts[:i+1], "/")))
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("secret sources must not contain symlinks")
		}
		if i == len(parts)-1 && !info.Mode().IsRegular() {
			return nil, errors.New("secret source must be a regular file")
		}
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("could not safely open secret source")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("secret source must be a regular file")
	}
	if info.Size() > projectsecret.MaxFileBytes {
		return nil, errors.New("secret file exceeds byte limit")
	}
	content, err := io.ReadAll(io.LimitReader(file, projectsecret.MaxFileBytes+1))
	if err != nil {
		return nil, errors.New("could not read secret source")
	}
	if len(content) > projectsecret.MaxFileBytes {
		return nil, errors.New("secret file exceeds byte limit")
	}
	return content, nil
}
