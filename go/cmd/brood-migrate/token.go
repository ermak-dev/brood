// Случайный пароль импортированной записи не выдаётся пользователю.
package main

import (
	"crypto/rand"
	"encoding/base64"
)

func newRandomPassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
