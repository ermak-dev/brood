package build

// FEATURE: Local current-file builds are independent from registry publication.
// SPEC: Load only the selected directory, copy safe current files, reconstruct
// metadata without credentials, export OCI atomically, and publish complete runs.
// IMPLEMENTATION: Root-confined reads, filtered working snapshots, shared buildx
// invocation, minimal OCI validation, and atomic archive/bundle publication.
// RESULT: IMPLEMENTED — current files build without Git or registry publication.
// STATUS: DONE

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func workingFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	data, err := os.ReadFile("testdata/minimal.yaml")
	wantNoError(t, err)
	wantNoError(t, os.WriteFile(filepath.Join(root, "Broodfile"), data, 0600))
	wantNoError(t, os.Mkdir(filepath.Join(root, "environment"), 0755))
	wantNoError(t, os.WriteFile(filepath.Join(root, "environment/Dockerfile"), []byte("FROM scratch\n"), 0644))
	return root
}

func testOCI(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tar.NewWriter(&b)
	for _, entry := range []struct{ name, text string }{{"oci-layout", `{"imageLayoutVersion":"1.0.0"}`}, {"index.json", `{"schemaVersion":2,"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:` + strings.Repeat("a", 64) + `","size":2}]}`}, {"blobs/sha256/" + strings.Repeat("a", 64), `{}`}} {
		wantNoError(t, w.WriteHeader(&tar.Header{Name: entry.name, Mode: 0644, Size: int64(len(entry.text))}))
		_, err := w.Write([]byte(entry.text))
		wantNoError(t, err)
	}
	wantNoError(t, w.Close())
	return b.Bytes()
}

func TestLoadWorkingProjectWithoutGitAndExport(t *testing.T) {
	root := workingFixture(t)
	for _, name := range []string{"changed", "untracked", ".env", ".env.production", "nested/.env", "secrets/key", "venv/file", "node_modules/file", ".git/config", ".brood/build/file", "inventory.yaml", "db.sqlite3", "brood.db", "brood.db-wal", "brood.db-shm"} {
		path := filepath.Join(root, name)
		wantNoError(t, os.MkdirAll(filepath.Dir(path), 0755))
		wantNoError(t, os.WriteFile(path, []byte(name), 0644))
	}
	wantNoError(t, os.Symlink("changed", filepath.Join(root, "safe")))
	p, err := LoadWorkingProject(root, "", "destination")
	wantNoError(t, err)
	if p.Root != root || !p.WorkingTree || p.Plan["atespace"] != "destination" || p.Plan["registry"] != "brood.local" {
		t.Fatalf("project: %+v", p)
	}
	out := filepath.Join(t.TempDir(), "context")
	reports, err := Export(p, out, nil)
	wantNoError(t, err)
	for _, name := range []string{"changed", "untracked", "safe"} {
		data, err := os.ReadFile(filepath.Join(out, "project", name))
		wantNoError(t, err)
		if len(data) == 0 {
			t.Fatal("empty copied file")
		}
	}
	for _, name := range []string{".env", ".env.production", "nested/.env", "secrets", "venv", "node_modules", ".git", ".brood", "inventory.yaml", "db.sqlite3", "brood.db", "brood.db-wal", "brood.db-shm"} {
		if _, err := os.Lstat(filepath.Join(out, "project", name)); !os.IsNotExist(err) {
			t.Fatalf("copied excluded %s", name)
		}
	}
	_, err = Render(p.Plan, "brood.local/example/python@sha256:"+strings.Repeat("a", 64), reports)
	wantNoError(t, err)
	_, err = LoadWorkingProject(filepath.Join(root, "environment"), "", "")
	if err == nil {
		t.Fatal("must not ascend to parent Broodfile")
	}
}

func TestWorkingProjectBoundedSafeDeclarationAndPins(t *testing.T) {
	for _, name := range []string{"Broodfile", "pins.json"} {
		t.Run(name, func(t *testing.T) {
			root := workingFixture(t)
			if name == "pins.json" {
				data, err := os.ReadFile(filepath.Join(root, "Broodfile"))
				wantNoError(t, err)
				wantNoError(t, os.WriteFile(filepath.Join(root, "Broodfile"), append(data, []byte("pinsFile: pins.json\n")...), 0600))
			}
			outside := filepath.Join(t.TempDir(), "outside")
			wantNoError(t, os.WriteFile(outside, []byte("{}"), 0600))
			os.Remove(filepath.Join(root, name))
			wantNoError(t, os.Symlink(outside, filepath.Join(root, name)))
			_, err := LoadWorkingProject(root, "", "")
			wantError(t, err, "escapes")
			wantNoError(t, os.Remove(filepath.Join(root, name)))
			wantNoError(t, os.WriteFile(filepath.Join(root, name), []byte(strings.Repeat("x", maxDocumentBytes+1)), 0600))
			_, err = LoadWorkingProject(root, "", "")
			wantError(t, err, "1 MiB")
		})
	}
	root := workingFixture(t)
	data, err := os.ReadFile(filepath.Join(root, "Broodfile"))
	wantNoError(t, err)
	text := strings.Replace(string(data), "platform: linux/amd64", "platform: linux/amd64\n      argsFromPins: [VERSION]", 1) + "pinsFile: pins.json\n"
	wantNoError(t, os.WriteFile(filepath.Join(root, "Broodfile"), []byte(text), 0600))
	wantNoError(t, os.WriteFile(filepath.Join(root, "pins.json"), []byte(`{"VERSION":"current"}`), 0600))
	p, err := LoadWorkingProject(root, "", "")
	wantNoError(t, err)
	if p.Plan["atespace"] != "example" || p.Plan["build"].(map[string]any)["arguments"].(map[string]any)["VERSION"] != "current" {
		t.Fatalf("plan: %v", p.Plan)
	}
	_, err = LoadWorkingProject(root, "", "../path")
	if err == nil {
		t.Fatal("destination must be a label")
	}
}

func TestWorkingSnapshotRejectsEscapesAndSpecialFiles(t *testing.T) {
	for _, kind := range []string{"link", "parent-link", "secret-link", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := workingFixture(t)
			target := filepath.Join(t.TempDir(), "snapshot")
			switch kind {
			case "link":
				wantNoError(t, os.Symlink(t.TempDir(), filepath.Join(root, "bad")))
			case "parent-link":
				wantNoError(t, os.Symlink(t.TempDir(), filepath.Join(root, "parent")))
				wantNoError(t, os.Symlink("parent/file", filepath.Join(root, "bad")))
			case "secret-link":
				wantNoError(t, os.WriteFile(filepath.Join(root, ".env"), []byte("secret"), 0600))
				wantNoError(t, os.Symlink(".env", filepath.Join(root, "bad")))
			case "fifo":
				wantNoError(t, syscall.Mkfifo(filepath.Join(root, "pipe"), 0600))
			}
			_, err := WorkingSnapshot(root, target, false)
			if err == nil {
				t.Fatal("unsafe source accepted")
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatal("partial snapshot published")
			}
		})
	}
}

func TestWorkingMetadataIsReconstructedAndCurrentFilesPreserved(t *testing.T) {
	f := newProjectFixture(t)
	f.write("code.py", "current")
	f.write("untracked", "new")
	f.git("config", "credential.helper", "do-not-copy")
	target := filepath.Join(f.root, "working")
	report, err := WorkingSnapshot(f.repo, target, true)
	wantNoError(t, err)
	if report["revision"] != f.sha {
		t.Fatalf("report: %v", report)
	}
	if f.read(t, target, "code.py") != "current" || f.read(t, target, "untracked") != "new" {
		t.Fatal("working changes lost")
	}
	data, err := os.ReadFile(filepath.Join(target, ".git/config"))
	wantNoError(t, err)
	if strings.Contains(string(data), "do-not-copy") || strings.Contains(string(data), "Fixture") {
		t.Fatal("local configuration leaked")
	}
	sha, err := Revision(target, "HEAD")
	wantNoError(t, err)
	if sha != f.sha {
		t.Fatal("HEAD lost")
	}
}

func TestWorkingMetadataRejectsExcludedCommittedBlobsAndParentRepositories(t *testing.T) {
	t.Run("committed-secret", func(t *testing.T) {
		f := newProjectFixture(t)
		f.write("nested/.env.production", "fixture secret")
		f.commit()
		_, err := WorkingSnapshot(f.repo, filepath.Join(f.root, "snapshot"), true)
		wantError(t, err, "excluded")
	})
	t.Run("parent-repository", func(t *testing.T) {
		f := newProjectFixture(t)
		_, err := WorkingSnapshot(filepath.Join(f.repo, "environment"), filepath.Join(f.root, "snapshot"), true)
		wantError(t, err, "root")
	})
}

func TestPublishLocalRefusesIncompletePreviousRun(t *testing.T) {
	root := workingFixture(t)
	p, err := LoadWorkingProject(root, "", "")
	wantNoError(t, err)
	artifacts, err := Render(p.Plan, "brood.local/example/python@sha256:"+strings.Repeat("a", 64), nil)
	wantNoError(t, err)
	archive := filepath.Join(t.TempDir(), "image.tar")
	wantNoError(t, os.WriteFile(archive, testOCI(t), 0600))
	output, err := PublishLocal(root, "development", artifacts, archive)
	wantNoError(t, err)
	wantNoError(t, os.Remove(filepath.Join(output, "egress-policy.json")))
	_, err = PublishLocal(root, "development", artifacts, archive)
	wantError(t, err, "complete")
}

func TestBackendBuildLocalArchiveAndAtomicFailures(t *testing.T) {
	root := workingFixture(t)
	p, err := LoadWorkingProject(root, "", "")
	wantNoError(t, err)
	archiveFixture := filepath.Join(t.TempDir(), "fixture.tar")
	wantNoError(t, os.WriteFile(archiveFixture, testOCI(t), 0600))
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$@" > %q
prev=
for arg in "$@"; do
 if [ "$prev" = '--metadata-file' ]; then printf '{"containerimage.digest":"sha256:%s"}' > "$arg"; fi
 if [ "$prev" = '--output' ]; then cp %q "${arg#type=oci,dest=}"; fi
 prev=$arg
done
`, calls, strings.Repeat("b", 64), archiveFixture)
	wantNoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0755))
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	path := filepath.Join(t.TempDir(), "image.oci.tar")
	options := BackendOptions{ArchivePath: path, OwnershipCheck: func() error { t.Fatal("local build checked ownership"); return nil }}
	result, err := BackendBuildLocal(p, options)
	wantNoError(t, err)
	if result.Archive != path || !strings.HasSuffix(result.Image, "@sha256:"+strings.Repeat("b", 64)) {
		t.Fatalf("result: %+v", result)
	}
	data, err := os.ReadFile(path)
	wantNoError(t, err)
	if !bytes.Equal(data, testOCI(t)) {
		t.Fatal("archive not persisted")
	}
	args, err := os.ReadFile(calls)
	wantNoError(t, err)
	if strings.Contains(string(args), "--push") || strings.Contains(string(args), "login") || !strings.Contains(string(args), "type=oci,dest=") {
		t.Fatalf("args: %s", args)
	}
	artifacts, err := Render(p.Plan, result.Image, result.Sources)
	wantNoError(t, err)
	out, err := PublishLocal(root, "development", artifacts, path)
	wantNoError(t, err)
	_, err = os.Stat(filepath.Join(out, "image.oci.tar"))
	wantNoError(t, err)
	_, err = PublishLocal(root, "development", artifacts, path)
	wantNoError(t, err)
	wantNoError(t, os.WriteFile(archiveFixture, []byte("invalid"), 0600))
	_, err = BackendBuildLocal(p, options)
	wantError(t, err, "OCI")
	data, err = os.ReadFile(path)
	wantNoError(t, err)
	if !bytes.Equal(data, testOCI(t)) {
		t.Fatal("failure replaced previous archive")
	}
	_, err = PublishLocal(root, "development", artifacts, archiveFixture)
	wantError(t, err, "OCI")
	data, err = os.ReadFile(filepath.Join(out, "image.oci.tar"))
	wantNoError(t, err)
	if !bytes.Equal(data, testOCI(t)) {
		t.Fatal("failure replaced previous run")
	}
	_, err = BackendBuildLocal(p, BackendOptions{})
	wantError(t, err, "ArchivePath")
}
