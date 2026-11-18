package relay

// WorkspaceBootstrapHeader negotiates the ephemeral workspace bootstrap format.
// Legacy guests omit it and must not receive the extended bootstrap object.
const WorkspaceBootstrapHeader = "X-Brood-Workspace-Protocol"

// WorkspaceBootstrapProtocol requires a guest supervisor that suppresses free
// autostart until exact source preparation and workspace attestation complete.
const WorkspaceBootstrapProtocol = "ephemeral-v1"
