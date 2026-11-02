//go:build linux

package guest

// BUG: Частичная установка оставляет старый manifest и блокирует следующие запуски.
// HYPOTHESIS: Нет durable write-ahead журнала допустимых старых/новых хешей.
// SOLUTION: Приватный fsync-журнал до эффектов, восстановление по старым/новым
// хешам и именам scratch-файлов, O_TMPFILE и flock для сериализации писателей.
// RESULT: FIXED — реальный поздний отказ и прерывания восстанавливаются с новым bundle.
// STATUS: FIXED

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"brood.dev/go/brood/projectsecret"
)

func TestProjectSecretJournalInterruptions(t *testing.T) {
	for _, op := range []string{"create", "update", "delete"} {
		for _, boundary := range []string{"journal", "prepared:a", "displaced:a", "installed:a", "cleaned:a", "manifest"} {
			if op == "delete" && boundary == "prepared:a" {
				continue
			}
			t.Run(op+"/"+boundary, func(t *testing.T) {
				root, home := t.TempDir(), t.TempDir()
				secretTestGit(t, root, "init", "-q")
				old := projectsecret.Bundle{Files: []projectsecret.File{{Name: "a", Data: []byte("synthetic-old")}}}
				if op != "create" {
					if _, err := applyProjectSecrets(root, home, old); err != nil {
						t.Fatal(err)
					}
				}
				desired := projectsecret.Bundle{Files: []projectsecret.File{{Name: "a", Data: []byte("synthetic-new")}, {Name: "z", Data: []byte("synthetic-z")}}, Vars: map[string]string{"SERVICE_KEY": "synthetic-env"}}
				if op == "delete" {
					desired.Files = desired.Files[1:]
				}
				reached := false
				_, err := applyProjectSecretsWithCheckpoint(root, home, desired, func(stage string) error {
					journalPath := filepath.Join(home, ".config/brood", secretJournalName)
					st, statErr := os.Stat(journalPath)
					if statErr != nil || st.Mode().Perm() != 0600 {
						t.Fatal("effects without private durable journal", statErr)
					}
					journal, _ := os.ReadFile(journalPath)
					for _, value := range []string{"synthetic-new", "synthetic-old", "synthetic-z", "synthetic-env", "SERVICE_KEY"} {
						if strings.Contains(string(journal), value) {
							t.Fatal("journal contains secret value or environment")
						}
					}
					if stage == boundary {
						reached = true
						return context.Canceled
					}
					return nil
				})
				if !reached || !errors.Is(err, context.Canceled) {
					t.Fatal("deterministic interruption not reached", err)
				}
				latest := projectsecret.Bundle{Files: []projectsecret.File{{Name: "a", Data: []byte("synthetic-latest")}, {Name: "current", Data: []byte("synthetic-current")}}}
				if _, err := applyProjectSecrets(root, home, latest); err != nil {
					t.Fatal("interrupted transaction cannot adopt latest bundle", err)
				}
				data, _ := os.ReadFile(filepath.Join(root, "a"))
				if string(data) != "synthetic-latest" {
					t.Fatal("latest bytes absent")
				}
				if _, err := os.Stat(filepath.Join(root, "z")); !os.IsNotExist(err) {
					t.Fatal("intermediate target retained")
				}
				entries, err := os.ReadDir(root)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".brood-secret-") {
						t.Fatal("scratch file leaked after recovery")
					}
				}
				if _, err := os.Stat(filepath.Join(home, ".config/brood", secretJournalName)); !os.IsNotExist(err) {
					t.Fatal("completed journal not retired")
				}
				if got := secretTestGit(t, root, "status", "--porcelain", "--untracked-files=all"); got != "" {
					t.Fatalf("Git exposes recovered secret bytes: %q", got)
				}
			})
		}
	}
}

func TestProjectSecretJournalRepeatedRetry(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(strconv.FormatBool(same), func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			secretTestGit(t, root, "init", "-q")
			bundle := projectsecret.Bundle{Files: []projectsecret.File{{Name: "a", Data: []byte("synthetic-one")}, {Name: "z", Data: []byte("synthetic-z")}}}
			for _, value := range []string{"synthetic-one", "synthetic-two"} {
				bundle.Files[0].Data = []byte(value)
				if _, err := applyProjectSecretsWithCheckpoint(root, home, bundle, func(stage string) error {
					if stage == "installed:a" {
						return context.Canceled
					}
					return nil
				}); !errors.Is(err, context.Canceled) {
					t.Fatal("repeated interruption failed", err)
				}
			}
			if !same {
				bundle = projectsecret.Bundle{}
			}
			if _, err := applyProjectSecrets(root, home, bundle); err != nil {
				t.Fatal("same or empty latest bundle failed to recover", err)
			}
			if same {
				data, _ := os.ReadFile(filepath.Join(root, "a"))
				if string(data) != "synthetic-two" {
					t.Fatal("same bundle recovery lost latest bytes")
				}
			} else {
				if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 || entries[0].Name() != ".git" {
					t.Fatal("empty latest bundle left owned files", err)
				}
			}
		})
	}
}

func TestProjectSecretJournalBoundsInterruptedUnion(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	secretTestGit(t, root, "init", "-q")
	parent := strings.Repeat("d", 240) + "/" + strings.Repeat("e", 240) + "/" + strings.Repeat("f", 240)
	makeBundle := func(prefix string, count int) projectsecret.Bundle {
		bundle := projectsecret.Bundle{}
		for i := 0; i < count; i++ {
			bundle.Files = append(bundle.Files, projectsecret.File{Name: prefix + "/" + parent + "/" + strings.Repeat("k", 80) + strconv.Itoa(i), Data: []byte("synthetic")})
		}
		return bundle
	}
	// One old path fits Git's exclude bound; the larger replacement fits its
	// own manifest, but their interrupted union must still be rejected first.
	old, next := makeBundle("old", 1), makeBundle("new", 74)
	union := secretManifest{}
	for _, bundle := range []projectsecret.Bundle{old, next} {
		manifest := secretManifest{}
		for _, file := range bundle.Files {
			manifest[file.Name] = secretHash(file.Data)
			union[file.Name] = secretHash(file.Data)
		}
		if data, err := json.Marshal(manifest); err != nil || len(data) > 64<<10 {
			t.Fatal("fixture exceeds individual manifest bound", err)
		}
	}
	if data, err := json.Marshal(union); err != nil || len(data) <= 64<<10 {
		t.Fatal("fixture does not exceed interrupted union bound", err)
	}
	if _, err := applyProjectSecrets(root, home, old); err != nil {
		t.Fatal("bounded old manifest failed", err)
	}
	if _, err := applyProjectSecrets(root, home, next); err == nil {
		t.Fatal("unrecoverably large union accepted")
	}
	for _, file := range old.Files {
		data, err := os.ReadFile(filepath.Join(root, file.Name))
		if err != nil || string(data) != "synthetic" {
			t.Fatal("union rejected after old-file effects")
		}
	}
	if _, err := os.Stat(filepath.Join(root, "new")); !os.IsNotExist(err) {
		t.Fatal("union rejected after new-file effects")
	}
}

func TestProjectSecretJournalPreservesEditedScratch(t *testing.T) {
	for _, which := range []string{"backup", "temp"} {
		t.Run(which, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			secretTestGit(t, root, "init", "-q")
			old := projectsecret.Bundle{Files: []projectsecret.File{{Name: "a", Data: []byte("synthetic-old")}}}
			if _, err := applyProjectSecrets(root, home, old); err != nil {
				t.Fatal(err)
			}
			desired := projectsecret.Bundle{Files: []projectsecret.File{{Name: "a", Data: []byte("synthetic-next")}}}
			if _, err := applyProjectSecretsWithCheckpoint(root, home, desired, func(stage string) error {
				if stage == "displaced:a" {
					return context.Canceled
				}
				return nil
			}); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			suffix := "-old"
			if which == "temp" {
				suffix = "-new"
			}
			path := filepath.Join(root, secretScratch("a", suffix))
			if err := os.WriteFile(path, []byte("user edit"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := applyProjectSecrets(root, home, projectsecret.Bundle{}); err == nil {
				t.Fatal("edited scratch accepted")
			}
			data, _ := os.ReadFile(path)
			if string(data) != "user edit" {
				t.Fatal("edited scratch removed")
			}
		})
	}
}

func TestProjectSecretJournalRejectsInvalidState(t *testing.T) {
	hash := secretHash([]byte("synthetic"))
	good := secretJournal{"a": {New: hash, Backup: secretScratch("a", "-old"), Temp: secretScratch("a", "-new")}}
	for _, kind := range []string{"null", "unknown", "oversized", "count", "path", "hash", "empty-hashes", "scratch", "alias", "permissions", "symlink", "hardlink", "manifest-mismatch"} {
		t.Run(kind, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			secretTestGit(t, root, "init", "-q")
			if _, err := applyProjectSecrets(root, home, projectsecret.Bundle{}); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(home, ".config/brood")
			name := filepath.Join(dir, secretJournalName)
			data, _ := json.Marshal(good)
			switch kind {
			case "null":
				data = []byte("null")
			case "unknown":
				data = []byte(`{"a":{"old":"","new":"` + hash + `","backup":"` + secretScratch("a", "-old") + `","temp":"` + secretScratch("a", "-new") + `","unexpected":"field"}}`)
			case "oversized":
				data = []byte(strings.Repeat(" ", secretJournalLimit+1))
			case "count":
				j := secretJournal{}
				for i := 0; i <= secretManagedLimit; i++ {
					n := "key" + strconv.Itoa(i)
					j[n] = secretJournalEntry{New: hash}
				}
				data, _ = json.Marshal(j)
			case "path":
				data, _ = json.Marshal(secretJournal{"../a": {New: hash}})
			case "hash":
				j := secretJournal{"a": {New: "bad", Backup: secretScratch("a", "-old"), Temp: secretScratch("a", "-new")}}
				data, _ = json.Marshal(j)
			case "empty-hashes":
				data, _ = json.Marshal(secretJournal{"a": {Backup: secretScratch("a", "-old"), Temp: secretScratch("a", "-new")}})
			case "scratch":
				data, _ = json.Marshal(secretJournal{"a": {New: hash, Backup: "elsewhere/old", Temp: "elsewhere/new"}})
			case "alias":
				j := secretJournal{}
				for n, e := range good {
					j[n] = e
				}
				j[secretScratch("a", "-old")] = secretJournalEntry{New: hash}
				data, _ = json.Marshal(j)
			case "manifest-mismatch":
				m, _ := json.Marshal(secretManifest{"a": secretHash([]byte("other"))})
				if err := os.WriteFile(filepath.Join(dir, "project-secrets.json"), m, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				if err := os.Symlink(filepath.Join(dir, "project-secrets.json"), name); err != nil {
					t.Fatal(err)
				}
			} else if kind == "hardlink" {
				source := filepath.Join(t.TempDir(), "journal")
				if err := os.WriteFile(source, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(source, name); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(name, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "permissions" {
				if err := os.Chmod(name, 0644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := applyProjectSecrets(root, home, projectsecret.Bundle{Files: []projectsecret.File{{Name: "key", Data: []byte("synthetic-next")}}}); err == nil {
				t.Fatal("invalid journal accepted")
			}
			if _, err := os.Stat(filepath.Join(root, "key")); !os.IsNotExist(err) {
				t.Fatal("bytes published with invalid journal")
			}
		})
	}
}

func TestProjectSecretConcurrentManifestWriters(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	secretTestGit(t, root, "init", "-q")
	entered, release := make(chan struct{}), make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := applyProjectSecretsWithCheckpoint(root, home, projectsecret.Bundle{Files: []projectsecret.File{{Name: "a", Data: []byte("synthetic-first")}}}, func(stage string) error {
			if stage == "journal" {
				close(entered)
				<-release
			}
			return nil
		})
		errs <- err
	}()
	<-entered
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := applyProjectSecrets(root, home, projectsecret.Bundle{Files: []projectsecret.File{{Name: "b", Data: []byte("synthetic-second")}}})
		errs <- err
	}()
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal("concurrent writer failed", err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(root, "b"))
	if string(data) != "synthetic-second" {
		t.Fatal("serialized latest write absent")
	}
	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Fatal("serialized old write retained")
	}
}

func TestProjectSecretRetryAfterLaterFilesystemFault(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission fault requires unprivileged test user")
	}
	for _, kind := range []string{"first-install", "update", "delete", "user-edit"} {
		t.Run(kind, func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()
			secretTestGit(t, root, "init", "-q")
			old := projectsecret.Bundle{Files: []projectsecret.File{{Name: "a", Data: []byte("synthetic-old")}}}
			if kind != "first-install" {
				if _, err := applyProjectSecrets(root, home, old); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(root, "z"), 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "z"), 0700) })
			failed := projectsecret.Bundle{Files: []projectsecret.File{{Name: "a", Data: []byte("synthetic-intermediate")}, {Name: "z/key", Data: []byte("synthetic-fault")}}}
			if kind == "delete" {
				failed.Files = failed.Files[1:]
			}
			if _, err := applyProjectSecrets(root, home, failed); err == nil {
				t.Fatal("later filesystem fault did not fail")
			}
			data, err := os.ReadFile(filepath.Join(root, "a"))
			if kind == "delete" {
				if !os.IsNotExist(err) {
					t.Fatal("delete did not precede actual fault")
				}
			} else if err != nil || string(data) != "synthetic-intermediate" {
				t.Fatal("first write did not precede actual fault")
			}
			if err := os.Chmod(filepath.Join(root, "z"), 0700); err != nil {
				t.Fatal(err)
			}
			latest := projectsecret.Bundle{Files: []projectsecret.File{{Name: "a", Data: []byte("synthetic-latest")}, {Name: "latest", Data: []byte("synthetic-current")}}}
			if kind == "user-edit" {
				if err := os.WriteFile(filepath.Join(root, "a"), []byte("user edit"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := applyProjectSecrets(root, home, latest); err == nil {
					t.Fatal("recovery clobbered user edit")
				}
				data, _ := os.ReadFile(filepath.Join(root, "a"))
				if string(data) != "user edit" {
					t.Fatal("user edit lost")
				}
				return
			}
			if _, err := applyProjectSecrets(root, home, latest); err != nil {
				t.Fatalf("corrected destination/latest bundle cannot recover: %v", err)
			}
			data, _ = os.ReadFile(filepath.Join(root, "a"))
			if string(data) != "synthetic-latest" {
				t.Fatal("latest bundle not installed")
			}
			if _, err := os.Stat(filepath.Join(root, "z/key")); !os.IsNotExist(err) {
				t.Fatal("obsolete interrupted target installed")
			}
			if _, err := applyProjectSecrets(root, home, projectsecret.Bundle{}); err != nil {
				t.Fatal("recovered removal failed", err)
			}
			if got := secretTestGit(t, root, "status", "--porcelain", "--untracked-files=all"); got != "" {
				t.Fatalf("secret artifacts remain visible: %q", got)
			}
		})
	}
}
