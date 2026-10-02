package agent

import (
	"testing"

	"github.com/timdebruijn/smokeng/internal/ingest"
)

// The master answers 400 to a batch with more rows than ingest.MaxBatchRows,
// and an agent discards a batch it is told cannot be decoded, because sending
// the same bytes again would wedge its outbox. Raising pushBatch past the
// master's cap would therefore lose measurements silently, so the two are tied.
func TestPushBatchStaysWithinWhatTheMasterAccepts(t *testing.T) {
	if pushBatch > ingest.MaxBatchRows {
		t.Fatalf("pushBatch = %d exceeds ingest.MaxBatchRows = %d: the master would refuse "+
			"every full batch and the agent would discard it", pushBatch, ingest.MaxBatchRows)
	}
}
