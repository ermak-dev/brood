//go:build linux

package guest

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestInboxPTYStartGatePipeRequiresOneExplicitRelease(t *testing.T) {
	for _, decision := range []string{"release", "close", "wrong byte"} {
		t.Run(decision, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			fd, err := unix.Dup(int(reader.Fd()))
			if err != nil {
				t.Fatal(err)
			}
			actor := &actorProcess{ptyStart: writer}
			switch decision {
			case "release":
				if err := actor.releasePTYStart(); err != nil {
					t.Fatal(err)
				}
				if err := actor.releasePTYStart(); err == nil {
					t.Fatal("a consumed startup gate released twice")
				}
			case "wrong byte":
				if _, err := writer.Write([]byte{0}); err != nil {
					t.Fatal(err)
				}
				fallthrough
			case "close":
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := awaitPTYStart(uintptr(fd)); (err == nil) != (decision == "release") {
				t.Fatalf("unexpected gate decision: %v", err)
			}
		})
	}
}
