package command

var sessionHistorySpec = spec("session_history", "Read actor session history with cursor pagination.", true, false, true,
	requiredText("name"), optionalText("cursor", ""), integer("limit", 100, 1, 1000))

// Terminal operations include writes, so the whole command is not read-only.
var sessionTerminalSpec = spec("session_terminal", "Open a private actor terminal attachment and read screen snapshots or send literal UTF-8 input; this is not a lossless PTY byte stream.", false, false, true,
	requiredText("name"), choice(requiredText("action"), "open", "read", "input", "interrupt", "resize", "close"),
	optionalText("tid", ""), choice(optionalText("mode", "ro"), "ro", "rw"),
	integer("cols", 100, 20, 240), integer("rows", 30, 5, 80),
	optionalText("cursor", ""), integer("wait", 0, 0, 15),
	integer("seq", 0, 0, 9007199254740991), optionalText("data", ""))
