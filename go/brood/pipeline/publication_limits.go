package pipeline

// Workflow publication limits bound document and request sizes.
const (
	MaxWorkflowDefinitions     = 256
	MaxWorkflowNameBytes       = 63
	MaxWorkflowDefinitionBytes = 1 << 20
	MaxWorkflowBatchBytes      = 4 << 20
	// MaxWorkflowBodyBytes includes JSON escaping and key overhead.
	MaxWorkflowBodyBytes = 6 * MaxWorkflowBatchBytes
)
