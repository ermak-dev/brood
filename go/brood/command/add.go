package command

// originArg is optional and absent by default: the server then inherits the
// creating master's origin or the project repository. Validation is server-side.
var originArg = Arg{Name: "origin", Type: "string", Description: "HTTPS Git origin to clone into the new agent's empty workspace; defaults to the creating master's origin, then the project repository", MaxLength: 1024}

var addSpec = spec("add", "Create an agent in the swarm. The agent gets its own container, and origin is cloned into its workspace.", false, false, true, requiredText("name"), originArg)
