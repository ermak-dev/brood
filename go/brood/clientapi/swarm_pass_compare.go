package clientapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// MaxPassComparisonBytes is the public comparison document limit, excluding
// the server's one trailing newline. It does not raise other API limits.
const MaxPassComparisonBytes = 32 << 20

// SwarmPassComparison is a read-only retained-history comparison. It does not
// assert eval provenance, semantic equivalence or permission to repeat work.
type SwarmPassComparison struct {
	Version        int                       `json:"version"`
	Alignment      string                    `json:"alignment"`
	Reference      SwarmPassComparisonRef    `json:"reference"`
	Candidate      SwarmPassComparisonRef    `json:"candidate"`
	SameGoal       bool                      `json:"sameGoal"`
	SameSource     bool                      `json:"sameSource"`
	SameDefinition bool                      `json:"sameDefinition"`
	StepCount      SwarmPassCountComparison  `json:"stepCount"`
	ChildTaskCount SwarmPassCountComparison  `json:"childTaskCount"`
	Steps          []SwarmPassStepComparison `json:"steps"`
}

// SwarmPassComparisonRef identifies one selected swarm and first visit sequence.
type SwarmPassComparisonRef struct {
	SwarmID string `json:"swarmId"`
	Pass    int64  `json:"pass"`
}

// SwarmPassCountComparison counts retained rows, not unrecorded work.
type SwarmPassCountComparison struct {
	Reference int64 `json:"reference"`
	Candidate int64 `json:"candidate"`
	Delta     int64 `json:"delta"`
}

// SwarmPassStepComparison aligns recorded positions, not global sequence IDs.
type SwarmPassStepComparison struct {
	Position            int                    `json:"position"`
	Reference           *SwarmPassComparedStep `json:"reference,omitempty"`
	Candidate           *SwarmPassComparedStep `json:"candidate,omitempty"`
	Node                string                 `json:"node"`
	Outcome             string                 `json:"outcome"`
	Report              string                 `json:"report"`
	ReportedOutcome     string                 `json:"reportedOutcome"`
	ReportedReport      string                 `json:"reportedReport"`
	DurationDeltaMillis *int64                 `json:"durationDeltaMillis,omitempty"`
}

// SwarmPassComparedStep keeps missing actual results separate from saved
// completion reports. Missing elapsed time is not a zero duration.
type SwarmPassComparedStep struct {
	Sequence        int64   `json:"sequence"`
	NodeID          string  `json:"nodeId"`
	State           string  `json:"state"`
	Outcome         *string `json:"outcome,omitempty"`
	Report          *string `json:"report,omitempty"`
	ReportedOutcome *string `json:"reportedOutcome,omitempty"`
	ReportedReport  *string `json:"reportedReport,omitempty"`
	DurationMillis  *int64  `json:"durationMillis,omitempty"`
	ChildTasks      int64   `json:"childTasks"`
}

// CompareSwarmPasses reads the exact selected pair through the shared origin,
// session and TLS transport. It never follows redirects, retries or mutates
// either pass. Refusal bodies are deliberately excluded from returned errors.
func (c *Client) CompareSwarmPasses(ctx context.Context, reference string, referencePass int64, candidate string, candidatePass int64) (SwarmPassComparison, error) {
	if reference == "" || candidate == "" || len(reference) > 256 || len(candidate) > 256 || referencePass < 1 || candidatePass < 1 {
		return SwarmPassComparison{}, errors.New("bounded swarm IDs and positive pass numbers are required")
	}
	query := url.Values{"swarm": {candidate}, "pass": {strconv.FormatInt(candidatePass, 10)}}
	path := "/api/swarms/" + url.PathEscape(reference) + "/passes/" + strconv.FormatInt(referencePass, 10) + "/compare?" + query.Encode()
	response, err := c.request(ctx, http.MethodGet, path, nil, 0, "", c.http.Timeout)
	if err != nil {
		return SwarmPassComparison{}, err
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return SwarmPassComparison{}, ErrRedirect
	case response.StatusCode == http.StatusUnauthorized:
		return SwarmPassComparison{}, ErrSessionExpired
	case response.StatusCode != http.StatusOK:
		return SwarmPassComparison{}, fmt.Errorf("pass comparison unavailable (status %d)", response.StatusCode)
	}
	c.absorbCookies(response)
	data, err := io.ReadAll(io.LimitReader(response.Body, MaxPassComparisonBytes+2))
	if err != nil {
		return SwarmPassComparison{}, errors.New("could not read pass comparison")
	}
	if len(data) > MaxPassComparisonBytes+1 || len(data) > MaxPassComparisonBytes && data[len(data)-1] != '\n' {
		return SwarmPassComparison{}, errors.New("pass comparison exceeds 32 MiB")
	}
	var result SwarmPassComparison
	if json.Unmarshal(data, &result) != nil || !validPassComparisonReply(result, reference, referencePass, candidate, candidatePass) {
		return SwarmPassComparison{}, errors.New("invalid pass comparison response")
	}
	return result, nil
}

func validPassComparisonReply(result SwarmPassComparison, reference string, referencePass int64, candidate string, candidatePass int64) bool {
	if result.Version != 1 || result.Alignment != "visit-position" || result.Reference != (SwarmPassComparisonRef{SwarmID: reference, Pass: referencePass}) || result.Candidate != (SwarmPassComparisonRef{SwarmID: candidate, Pass: candidatePass}) {
		return false
	}
	if !validComparisonCounts(result.StepCount) || !validComparisonCounts(result.ChildTaskCount) || result.StepCount.Reference < 1 || result.StepCount.Candidate < 1 || result.StepCount.Reference > 4096 || result.StepCount.Candidate > 4096 || int64(len(result.Steps)) != max(result.StepCount.Reference, result.StepCount.Candidate) {
		return false
	}
	var referenceCount, candidateCount, referenceChildren, candidateChildren int64
	previousReference, previousCandidate := referencePass-1, candidatePass-1
	for index, row := range result.Steps {
		if row.Position != index+1 || row.Reference == nil && row.Candidate == nil {
			return false
		}
		if row.Reference != nil {
			if !validComparedStep(row.Reference, previousReference) || index == 0 && row.Reference.Sequence != referencePass {
				return false
			}
			previousReference = row.Reference.Sequence
			referenceCount++
			referenceChildren += row.Reference.ChildTasks
		}
		if row.Candidate != nil {
			if !validComparedStep(row.Candidate, previousCandidate) || index == 0 && row.Candidate.Sequence != candidatePass {
				return false
			}
			previousCandidate = row.Candidate.Sequence
			candidateCount++
			candidateChildren += row.Candidate.ChildTasks
		}
		for _, status := range []string{row.Outcome, row.Report, row.ReportedOutcome, row.ReportedReport} {
			if status != "same" && status != "different" && status != "unknown" && status != "not_comparable" {
				return false
			}
		}
		switch row.Node {
		case "same":
			if row.Reference == nil || row.Candidate == nil || row.Reference.NodeID != row.Candidate.NodeID {
				return false
			}
		case "different":
			if row.Reference == nil || row.Candidate == nil || row.Reference.NodeID == row.Candidate.NodeID {
				return false
			}
		case "missing":
			if row.Reference != nil && row.Candidate != nil {
				return false
			}
		default:
			return false
		}
		if row.Node != "same" && (row.Outcome != "not_comparable" || row.Report != "not_comparable" || row.ReportedOutcome != "not_comparable" || row.ReportedReport != "not_comparable") {
			return false
		}
		if row.DurationDeltaMillis != nil {
			if row.Node != "same" || row.Reference.DurationMillis == nil || row.Candidate.DurationMillis == nil || *row.DurationDeltaMillis != *row.Candidate.DurationMillis-*row.Reference.DurationMillis {
				return false
			}
		}
	}
	return referenceCount == result.StepCount.Reference && candidateCount == result.StepCount.Candidate && referenceChildren == result.ChildTaskCount.Reference && candidateChildren == result.ChildTaskCount.Candidate
}

func validComparisonCounts(count SwarmPassCountComparison) bool {
	return count.Reference >= 0 && count.Reference <= 1<<30 && count.Candidate >= 0 && count.Candidate <= 1<<30 && count.Delta == count.Candidate-count.Reference
}

func validComparedStep(step *SwarmPassComparedStep, previous int64) bool {
	return step.Sequence > previous && step.NodeID != "" && step.State != "" && step.ChildTasks >= 0 && step.ChildTasks <= 1<<30 && (step.DurationMillis == nil || *step.DurationMillis >= 0 && *step.DurationMillis <= 9223372036854)
}
