package store

import _ "embed"

//go:embed project_secret.sql
var projectSecretSchema string

// Регистрация секретов в общей схеме — schemaModules ("project-secret")
// и verifyProjectSecretSchema в registry.go (#205).
