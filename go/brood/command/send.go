package command

import "time"

var sendSpec = spec("send", "Send a message to an actor and return delivery status, not an assistant response; optionally wait for an idle notice.", false, false, true,
	requiredText("to"), requiredText("message"), integer("wait_seconds", 0, 0, 300))

// ReceiptWait is how long the recipient guest waits for the Claude Code
// delivery receipt when a message does not wait for idle (wait 0, #260).
// Claude Code confirms a message after it is accepted by the session, not
// at once, so a short wait reports a delivered message as unconfirmed. The
// limit also bounds how long an operator answer holds its request.
const ReceiptWait = 15 * time.Second

// ReplyWait is how long the recipient guest waits after the message write:
// the delivery receipt up to ReceiptWait without an idle wait, otherwise up
// to wait seconds for idle, as before.
func ReplyWait(wait int) time.Duration {
	if wait <= 0 {
		return ReceiptWait
	}
	return time.Duration(wait) * time.Second
}

// CallTimeout bounds one guest command call at every hop of its channel
// (MCP adapter, bro --guest, guest broker, relay on both ends, server): the
// reply wait plus 15 seconds for the helper start, transfer and verdict. The
// recipient guest answers within ReplyWait plus 10 seconds.
func CallTimeout(wait int) time.Duration {
	return ReplyWait(wait) + 15*time.Second
}
