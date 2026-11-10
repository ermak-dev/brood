//go:build linux

package sessionctl

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"brood.dev/go/brood/gitorigin"
	"brood.dev/go/brood/proxmox/relay"
)

// CheckoutOperation is internal to the guest root: the relay handler refuses it
// from the server, and root runs it only while no actor process exists.
const CheckoutOperation = "workspace_checkout"

// GatewayURL is the clone URL of the root Git gateway in gateway mode.
const GatewayURL = "http://127.0.0.1:8318/origin.git"

const checkoutBudget = 8 * time.Minute
const checkoutFetchTimeout = 5 * time.Minute
const checkoutStepTimeout = 2 * time.Minute
const snapshotObjectLimit = 2 * workspaceFileLimit
const snapshotByteLimit = 4 << 30
const commitObjectLimit = 1 << 20

// CheckoutRequest is filled by the guest root from its own bootstrap origin.
type CheckoutRequest struct {
	Commit string `json:"commit"`
	Origin string `json:"origin"`
	Mode   string `json:"mode"`
}

// CheckoutFacts reports a verified detached checkout of Commit.
type CheckoutFacts struct {
	Commit  string `json:"commit"`
	Fetched bool   `json:"fetched"`
}

// RunCheckout brings the fixed clone to a commit inside the unprivileged helper.
// The root caller must guarantee that no other actor process exists meanwhile:
// only then is the helper's view of the workspace not forgeable by the model.
func RunCheckout(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	if err := requireActorIdentity("checkout helper"); err != nil {
		return failure(err.Error())
	}
	if req.Operation != CheckoutOperation {
		return relay.SessionResponse{Text: "unsupported checkout operation", Outcome: "unsupported"}
	}
	r, err := parseCheckoutRequest(req.Arguments)
	if err != nil {
		return failure(err.Error())
	}
	ctx, cancel := context.WithTimeout(ctx, checkoutBudget)
	defer cancel()
	facts, err := newCheckoutEngine(r).run(ctx, r.Commit)
	if err != nil {
		return failure(err.Error())
	}
	return success(facts)
}

func parseCheckoutRequest(raw []byte) (CheckoutRequest, error) {
	var r CheckoutRequest
	if decodeArgs(raw, &r) != nil {
		return r, errors.New("invalid checkout arguments; only commit, origin and mode are accepted")
	}
	commit, err := hex.DecodeString(r.Commit)
	if err != nil || len(commit) != 20 || r.Commit != strings.ToLower(r.Commit) {
		return r, errors.New("checkout commit must be a full lowercase SHA-1")
	}
	if canonical, err := gitorigin.Canonical(r.Origin); err != nil || canonical != r.Origin {
		return r, errors.New("checkout origin must be a canonical HTTPS origin")
	}
	if r.Mode != "gateway" && r.Mode != "direct" {
		return r, errors.New("checkout origin mode must be gateway or direct")
	}
	return r, nil
}

// checkoutEngine fixes the clone, the fetch source and its transport. Tests
// replace the source with a local repository and the "file" protocol, and
// home with a directory they own.
type checkoutEngine struct {
	root     string
	home     string
	scratch  string
	source   string
	protocol string
	proxied  bool
}

func newCheckoutEngine(r CheckoutRequest) *checkoutEngine {
	e := &checkoutEngine{root: fixedWorkspace, home: actorHome, source: GatewayURL, protocol: "http"}
	if r.Mode == "direct" {
		e.source, e.protocol, e.proxied = r.Origin, "https", true
	}
	return e
}

// run refuses before any effect when the clone is unsafe or has local work.
// Only after fetch and snapshot verification does it move HEAD; a detached
// checkout never deletes a branch, a commit or an untracked file, and writes
// the bytes of the commit without .gitattributes conversions.
func (e *checkoutEngine) run(ctx context.Context, commit string) (CheckoutFacts, error) {
	if err := e.repository(ctx); err != nil {
		return CheckoutFacts{}, fmt.Errorf("%s; nothing changed", err)
	}
	changes, err := e.changes(ctx, "HEAD")
	if err != nil {
		return CheckoutFacts{}, fmt.Errorf("%s; nothing changed", err)
	}
	if changes != "" {
		return CheckoutFacts{}, fmt.Errorf("workspace has local changes (%s; compared byte for byte, without .gitattributes conversions); commit or remove them, nothing changed", changes)
	}
	facts := CheckoutFacts{Commit: commit}
	if !e.has(ctx, commit) {
		e.fetch(ctx, commit)
		if !e.has(ctx, commit) {
			return CheckoutFacts{}, errors.New("commit is unavailable from the workspace origin or its local object is corrupt; HEAD and files unchanged")
		}
		facts.Fetched = true
	}
	// Git не перепроверяет хэш объекта при чтении: подложенный объект иначе
	// попал бы в рабочее дерево под чужим именем.
	if err := e.verifySnapshot(ctx, commit); err != nil {
		return CheckoutFacts{}, fmt.Errorf("%s; HEAD and files unchanged", err)
	}
	if !e.detachedAt(commit) {
		if _, err := e.git(ctx, checkoutStepTimeout, nil, "checkout", "--quiet", "--detach", "--no-overwrite-ignore", "--no-recurse-submodules", commit, "--"); err != nil {
			return CheckoutFacts{}, errors.New("checkout refused, for example by an ignored file at a tracked path of the commit; local files preserved")
		}
	}
	if !e.detachedAt(commit) {
		return CheckoutFacts{}, errors.New("HEAD is not detached at the commit after checkout")
	}
	changes, err = e.changes(ctx, commit)
	if err != nil {
		return CheckoutFacts{}, fmt.Errorf("checkout is not verified: %s", err)
	}
	if changes != "" {
		return CheckoutFacts{}, fmt.Errorf("checkout is not verified: workspace differs from the commit byte for byte (%s)", changes)
	}
	return facts, nil
}

// repository adds to the shared layout checks what a checkout needs: no
// repository attributes (they rewrite content before hashing) and no gitlink
// anywhere in the index, streamed because real indexes exceed output limits.
func (e *checkoutEngine) repository(ctx context.Context) error {
	quick := func(ctx context.Context, args ...string) (string, error) {
		return e.git(ctx, checkoutStepTimeout, nil, args...)
	}
	if err := repositoryLayout(ctx, e.root, quick, false, []string{"info/attributes"}); err != nil {
		return err
	}
	return e.stream(ctx, checkoutStepTimeout, nil, nil, []string{"ls-files", "--stage", "-z"}, func(r *bufio.Reader) error {
		for {
			entry, err := r.ReadString(0)
			if strings.HasPrefix(entry, "160000 ") {
				return errors.New("submodule workspaces are not supported")
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return errors.New("Git index unavailable")
			}
		}
	})
}

// changes compares the workspace with rev by content: a temporary index built
// from rev has no stat cache, assume-unchanged or skip-worktree bits, so Git
// must hash every tracked file, as is (gitFlags byteExact). The real index is
// compared separately.
func (e *checkoutEngine) changes(ctx context.Context, rev string) (string, error) {
	staged, err := e.git(ctx, checkoutStepTimeout, nil, "diff-index", "--cached", "--name-only", "-z", rev, "--")
	if err != nil {
		return "", errors.New("Git index cannot be compared or exceeds output limit")
	}
	dir, err := os.MkdirTemp(e.scratch, "brood-checkout-")
	if err != nil {
		return "", errors.New("private scratch directory unavailable")
	}
	defer os.RemoveAll(dir)
	index := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	if _, err := e.git(ctx, checkoutStepTimeout, index, "read-tree", rev); err != nil {
		return "", errors.New("snapshot index cannot be built")
	}
	status, err := e.git(ctx, checkoutStepTimeout, index, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return "", errors.New("Git status unavailable or exceeds output limit")
	}
	var found []string
	if n := countEntries(staged, ""); n > 0 {
		found = append(found, strconv.Itoa(n)+" staged")
	}
	if n := countEntries(status, "?? "); n > 0 {
		found = append(found, strconv.Itoa(n)+" untracked")
	}
	if n := countEntries(status, "") - countEntries(status, "?? "); n > 0 {
		found = append(found, strconv.Itoa(n)+" modified tracked")
	}
	return strings.Join(found, ", "), nil
}

func countEntries(list, prefix string) int {
	n := 0
	for _, entry := range strings.Split(list, "\x00") {
		if entry != "" && strings.HasPrefix(entry, prefix) {
			n++
		}
	}
	return n
}

func (e *checkoutEngine) has(ctx context.Context, commit string) bool {
	_, err := e.git(ctx, checkoutStepTimeout, nil, "cat-file", "-e", commit+"^{commit}")
	return err == nil
}

// fetch asks the root-chosen source for the exact commit, then for branch tips
// when the server refuses unadvertised objects. The caller checks the result.
func (e *checkoutEngine) fetch(ctx context.Context, commit string) {
	env := []string{"GIT_ALLOW_PROTOCOL=" + e.protocol}
	if e.proxied {
		env = append(env, "HTTPS_PROXY=http://127.0.0.1:3128", "https_proxy=http://127.0.0.1:3128", "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	}
	args := []string{"-c", "protocol." + e.protocol + ".allow=always", "-c", "http.followRedirects=false", "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--", e.source}
	if _, err := e.git(ctx, checkoutFetchTimeout, env, append(args, commit)...); err == nil && e.has(ctx, commit) {
		return
	}
	_, _ = e.git(ctx, checkoutFetchTimeout, env, append(args, "+refs/heads/*:refs/remotes/origin/*")...)
}

// detachedAt reads HEAD itself: a symbolic ref resolving to the commit is a
// branch the next commit would move, not a detached checkout.
func (e *checkoutEngine) detachedAt(commit string) bool {
	head, err := readSafe(filepath.Join(e.root, ".git", "HEAD"), 128)
	return err == nil && string(head) == commit+"\n"
}

type snapshotObject struct{ oid, kind string }

// verifySnapshot hashes the commit, its root tree and every tree and blob of
// the snapshot. Each listed object is named by an already verified parent, so
// the whole snapshot is bound to the commit ID.
func (e *checkoutEngine) verifySnapshot(ctx context.Context, commit string) error {
	tree, err := e.git(ctx, checkoutStepTimeout, nil, "rev-parse", "--verify", "--end-of-options", commit+"^{tree}")
	if err != nil {
		return errors.New("commit tree cannot be resolved")
	}
	tree = strings.TrimSpace(tree)
	objects := []snapshotObject{{commit, "commit"}, {tree, "tree"}}
	seen := map[string]bool{commit: true, tree: true}
	err = e.stream(ctx, checkoutStepTimeout, nil, nil, []string{"ls-tree", "-r", "-t", "-z", "--full-tree", commit}, func(r *bufio.Reader) error {
		for {
			entry, err := r.ReadString(0)
			if err == io.EOF && entry == "" {
				return nil
			}
			if err != nil {
				return errors.New("snapshot listing is truncated")
			}
			meta, _, ok := strings.Cut(entry, "\t")
			fields := strings.Fields(meta)
			if !ok || len(fields) != 3 {
				return errors.New("snapshot listing is malformed")
			}
			if fields[1] == "commit" {
				return errors.New("submodule snapshots are not supported")
			}
			if fields[1] != "tree" && fields[1] != "blob" {
				return errors.New("snapshot listing has an unexpected object type")
			}
			if !seen[fields[2]] {
				seen[fields[2]] = true
				objects = append(objects, snapshotObject{fields[2], fields[1]})
			}
			if len(objects) > snapshotObjectLimit {
				return errors.New("snapshot exceeds the object limit")
			}
		}
	})
	if err != nil {
		return err
	}
	var input strings.Builder
	for _, o := range objects {
		input.WriteString(o.oid + "\n")
	}
	return e.stream(ctx, checkoutStepTimeout, nil, strings.NewReader(input.String()), []string{"cat-file", "--batch"}, func(r *bufio.Reader) error {
		total := int64(0)
		for _, want := range objects {
			if err := hashObject(r, want, tree, &total); err != nil {
				return err
			}
		}
		return nil
	})
}

func hashObject(r *bufio.Reader, want snapshotObject, tree string, total *int64) error {
	header, err := r.ReadString('\n')
	fields := strings.Fields(header)
	if err != nil || len(fields) != 3 || fields[0] != want.oid || fields[1] != want.kind {
		return errors.New("snapshot object is missing or has an unexpected type")
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if *total += size; err != nil || size < 0 || *total > snapshotByteLimit || (want.kind == "commit" && size > commitObjectLimit) {
		return errors.New("snapshot exceeds the size limit")
	}
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", want.kind, size)
	body := io.TeeReader(io.LimitReader(r, size), h)
	if want.kind == "commit" {
		data, err := io.ReadAll(body)
		if err != nil || int64(len(data)) != size || !strings.HasPrefix(string(data), "tree "+tree+"\n") {
			return errors.New("snapshot commit object hash mismatch")
		}
	} else if n, err := io.Copy(io.Discard, body); err != nil || n != size {
		return errors.New("snapshot object is truncated")
	}
	if end, err := r.ReadByte(); err != nil || end != '\n' {
		return errors.New("snapshot object stream is malformed")
	}
	if hex.EncodeToString(h.Sum(nil)) != want.oid {
		return errors.New("snapshot object hash mismatch")
	}
	return nil
}

func (e *checkoutEngine) command(ctx context.Context, env []string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/usr/bin/git", append(gitFlags(true), args...)...)
	cmd.Dir = e.root
	// Повтор ключа в Env: os/exec берёт последнее значение.
	cmd.Env = append(gitEnv(e.home, e.root), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	return cmd
}

// git runs one bounded command with capped output.
func (e *checkoutEngine) git(parent context.Context, timeout time.Duration, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := e.command(ctx, env, args)
	output := &cappedOutput{cancel: cancel}
	cmd.Stdout = output
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return output.String(), err
}

// stream hands stdout to consume; an early consumer error stops Git.
func (e *checkoutEngine) stream(parent context.Context, timeout time.Duration, env []string, stdin io.Reader, args []string, consume func(*bufio.Reader) error) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := e.command(ctx, env, args)
	cmd.Stdin = stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.New("Git output unavailable")
	}
	if err := cmd.Start(); err != nil {
		return errors.New("Git unavailable")
	}
	consumed := consume(bufio.NewReaderSize(stdout, 64<<10))
	if consumed != nil {
		cancel()
	}
	waited := cmd.Wait()
	if consumed != nil {
		return consumed
	}
	if ctx.Err() != nil {
		return errors.New("Git timed out")
	}
	if waited != nil {
		return errors.New("Git failed")
	}
	return nil
}
