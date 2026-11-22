package command

// MaxArtifactBytes fits a base64-encoded artifact in the swarm relay frame.
const MaxArtifactBytes = 64 << 10

var artifactPublishSpec = spec("artifact_publish", "Publish a private, immutable artifact version in the current swarm. Supply the file bytes as content_base64 (CLI: --file PATH instead). Does not notify the operator.", false, false, true,
	requiredText("name"), requiredText("media_type"), Arg{Name: "content_base64", Type: "string", Required: true, MaxLength: (MaxArtifactBytes + 2) / 3 * 4})

var artifactGetSpec = spec("artifact_get", "Get the metadata and base64 contents of an exact artifact version in the current swarm. Pass its version_id from the publication or the last segment of its link.", true, false, true,
	Arg{Name: "version_id", Type: "string", Required: true, Positional: true, MaxLength: 64})
