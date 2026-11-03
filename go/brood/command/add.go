package command

// originArg is optional and absent by default: the server then inherits the
// creating master's origin or the project repository. Validation is server-side.
var originArg = Arg{Name: "origin", Type: "string", Description: "HTTPS Git origin to clone into the new actor's empty workspace; defaults to the master's origin, then the project repository", MaxLength: 1024}

var addSpec = spec("add", "Create an actor in the selected scope.", false, false, true, requiredText("name"), originArg)
