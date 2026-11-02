package command

var projectAddSpec = spec("project_add", "Create a project with an optional description.", false, false, false,
	requiredText("name"), optionalText("description", ""))

// Omitted actors reads the limit; zero sets a zero actor limit, not unlimited.
var projectLimitSpec = spec("project_limit", "Read the project actor limit, or set it when actors is supplied. Zero permits no actors.", false, false, false,
	optionalText("name", ""), integer("actors", nil, 0, 9007199254740991))
