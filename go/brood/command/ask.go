package command

// askOperatorSpec opens an operator request for the calling actor (#210). The
// server takes the actor identity from the relay, never from arguments; the
// answer arrives later as an operator message and never routes the graph.
var askOperatorSpec = spec("ask_operator", "Ask the human operator of your swarm a question. The answer arrives later as a message from the operator; it never routes the workflow graph.", false, false, true,
	Arg{Name: "question", Type: "string", Required: true, Positional: true, MaxLength: 16384},
	choice(Arg{Name: "kind", Type: "string", Default: "text", Description: "text: free answer; confirm: yes or no; choice: one of options"}, "text", "confirm", "choice"),
	Arg{Name: "context", Type: "string", Default: "", MaxLength: 16384, Description: "Material the question is about"},
	Arg{Name: "options", Type: "string", Default: "", MaxLength: 16384, Description: "Choice labels, one per line; only for kind=choice"},
)
