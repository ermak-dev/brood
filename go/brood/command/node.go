package command

var nodeSpec = spec("node", "List worker nodes.", true, false, false)
var nodeDrainSpec = spec("node_drain", "Drain a worker node to prevent new actor placement.", false, false, false, requiredText("name"))
var nodeUpSpec = spec("node_up", "Make a drained worker node available for placement.", false, false, false, requiredText("name"))
