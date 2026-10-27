// Идентификаторы записей: ULID, монотонный в пределах процесса и
// сортируемый лексикографически.
package store

import (
	"crypto/rand"
	"sync"
	"time"
)

// ulidAlphabet — Crockford base32 без неоднозначных символов.
const ulidAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	ulidMu   sync.Mutex
	ulidLast time.Time
)

// NewID возвращает ULID: 10 символов времени (миллисекунды) и 16
// символов случайности. Достаточно уникален для всех таблиц store;
// вызывающий может передать и свой ID — store его не генерирует.
func NewID() string {
	now := time.Now().UTC()

	ulidMu.Lock()
	if !now.After(ulidLast) {
		// Та же миллисекунда: сдвиг вперёд держит монотонность и
		// лексикографический порядок.
		now = ulidLast.Add(time.Millisecond)
	}
	ulidLast = now
	ulidMu.Unlock()

	ms := uint64(now.UnixMilli())
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Без crypto/rand идентификаторам нельзя доверять; паника —
		// отказ неработающего источника энтропии, а не прикладной отказ.
		panic("store: crypto/rand failed: " + err.Error())
	}

	var id [26]byte
	for i := 9; i >= 0; i-- {
		id[i] = ulidAlphabet[ms%32]
		ms /= 32
	}
	for i := 0; i < 16; i++ {
		id[10+i] = ulidAlphabet[b[i]%32]
	}
	return string(id[:])
}
