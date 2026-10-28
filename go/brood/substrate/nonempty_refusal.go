package substrate

import "errors"

// ErrAtespaceNotEmpty identifies an atomic namespace-delete refusal due to children.
// The wrapped APIError retains its existing HTTP status and upstream detail.
var ErrAtespaceNotEmpty = errors.New("atespace is not empty")
