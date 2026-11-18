package store

import (
	"encoding/json"
	"errors"
)

// MaxPassTraceBytes bounds retained JSON material collected in one snapshot.
// The collector refuses the whole trace rather than truncating a frame or report.
const MaxPassTraceBytes = 32 << 20

// MaxPassTraceRecords bounds the combined number of visits and questions.
const MaxPassTraceRecords = 4096

// ErrPassTraceLimit means no partial trace is available within the export budget.
var ErrPassTraceLimit = errors.New("pass trace exceeds 4096 records or 32 MiB of material; no partial trace was returned")

type passTraceBudget struct {
	used, records int
}

func (b *passTraceBudget) add(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded) > MaxPassTraceBytes-b.used {
		return ErrPassTraceLimit
	}
	b.used += len(encoded)
	return nil
}

func (b *passTraceBudget) record(value any) error {
	if b.records >= MaxPassTraceRecords {
		return ErrPassTraceLimit
	}
	b.records++
	return b.add(value)
}
