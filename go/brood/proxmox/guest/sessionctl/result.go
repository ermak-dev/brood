//go:build linux

package sessionctl

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"brood.dev/go/brood/proxmox/relay"
)

// ResultOperation is internal to a root-supervised quiescent observation window.
const ResultOperation = "workspace_observe_result"

// RunResult observes the fixed clone without checkout, fetch or cleanup effects.
// The root supervisor must guarantee quiescence and authenticate any returned facts.
func RunResult(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
	if err := requireActorIdentity("result observation helper"); err != nil {
		return failure(err.Error())
	}
	if req.Operation != ResultOperation {
		return relay.SessionResponse{Text: "unsupported result operation", Outcome: "unsupported"}
	}
	request, err := parseCheckoutRequest(req.Arguments)
	if err != nil {
		return failure("invalid result observation arguments")
	}
	ctx, cancel := context.WithTimeout(ctx, checkoutBudget)
	defer cancel()
	facts, err := newCheckoutEngine(request).observeResult(ctx, request.Commit)
	if err != nil {
		return failure(err.Error())
	}
	return success(facts)
}

const resultPathLimit = 2048
const resultPathByteLimit = 64 << 10

// ResultFacts is a complete observed committed diff, not a signed root assertion.
// The root caller must independently guarantee no actor process exists throughout.
// Dirty, unsupported or over-limit workspaces return no facts, never a partial diff.
type ResultFacts struct {
	Base  string   `json:"base"`
	Head  string   `json:"head"`
	Paths []string `json:"paths"`
}

func (e *checkoutEngine) observeResult(ctx context.Context, base string) (ResultFacts, error) {
	if !resultCommit(base) {
		return ResultFacts{}, errors.New("result base must be a full lowercase SHA-1")
	}
	if err := e.repository(ctx); err != nil {
		return ResultFacts{}, err
	}
	raw, err := e.git(ctx, checkoutStepTimeout, nil, "rev-parse", "--verify", "HEAD^{commit}")
	head := strings.TrimSpace(raw)
	if err != nil || !resultCommit(head) {
		return ResultFacts{}, errors.New("result HEAD is unavailable")
	}
	for _, commit := range []string{base, head} {
		if err := e.verifySnapshot(ctx, commit); err != nil {
			return ResultFacts{}, errors.New("result base or HEAD snapshot cannot be verified")
		}
	}
	changes, err := e.changes(ctx, head)
	if err != nil || changes != "" {
		return ResultFacts{}, errors.New("result requires a clean committed workspace; local changes or unknown evidence remain")
	}
	var paths []string
	err = e.stream(ctx, checkoutStepTimeout, nil, nil, []string{"diff-tree", "--no-commit-id", "--name-status", "-r", "-z", "--no-ext-diff", "--no-textconv", "-M", "-C", "--find-copies-harder", base, head, "--"}, func(reader *bufio.Reader) error {
		var err error
		paths, err = resultPaths(reader)
		return err
	})
	if err != nil {
		return ResultFacts{}, errors.New("result diff is unavailable, malformed or exceeds the evidence limit")
	}
	after, err := e.git(ctx, checkoutStepTimeout, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || strings.TrimSpace(after) != head {
		return ResultFacts{}, errors.New("result HEAD changed during observation")
	}
	changes, err = e.changes(ctx, head)
	if err != nil || changes != "" {
		return ResultFacts{}, errors.New("result workspace changed during observation")
	}
	return ResultFacts{Base: base, Head: head, Paths: paths}, nil
}

func resultCommit(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 20 && value == strings.ToLower(value)
}

func resultPath(value string) bool {
	if value == "" || len(value) > 1024 || !utf8.ValidString(value) || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return false
		}
	}
	parts := strings.Split(value, "/")
	if len(parts) > 128 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func resultPaths(reader *bufio.Reader) ([]string, error) {
	invalid := errors.New("invalid or over-limit result path evidence")
	var total int
	token := func(limit int) (string, error) {
		raw, err := reader.ReadSlice(0)
		if err == io.EOF && len(raw) == 0 {
			return "", io.EOF
		}
		if err != nil || len(raw) > limit+1 {
			return "", invalid
		}
		total += len(raw)
		if total > resultPathByteLimit {
			return "", invalid
		}
		return string(raw[:len(raw)-1]), nil
	}
	paths := []string{}
	for records := 0; ; records++ {
		status, err := token(4)
		if err == io.EOF {
			break
		}
		if err != nil || records >= resultPathLimit {
			return nil, invalid
		}
		count := 1
		switch status {
		case "A", "D", "M", "T":
		default:
			if len(status) < 2 || status[0] != 'R' && status[0] != 'C' {
				return nil, invalid
			}
			score, err := strconv.Atoi(status[1:])
			if err != nil || score < 0 || score > 100 {
				return nil, invalid
			}
			count = 2
		}
		for range count {
			path, err := token(1024)
			if err != nil || !resultPath(path) {
				return nil, invalid
			}
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	if len(paths) > resultPathLimit {
		return nil, invalid
	}
	return paths, nil
}
