package server

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

const headTestCommit = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// pktLine оборачивает payload в одну pkt-line smart HTTP.
func pktLine(payload string) string { return fmt.Sprintf("%04x", len(payload)+4) + payload }

// headAdvertisement — объявление upload-pack протокола v0, первая строка
// которого — ref с capabilities caps.
func headAdvertisement(ref, caps string) string {
	return pktLine("# service=git-upload-pack\n") + "0000" + pktLine(ref+"\x00"+caps+"\n") + pktLine(headTestCommit+" refs/heads/trunk\n") + "0000"
}

const headTestCaps = "multi_ack thin-pack side-band-64k ofs-delta symref=HEAD:refs/heads/trunk object-format=sha1 agent=git/2.43.0"

// failingReader отвечает на каждое чтение ошибкой ввода-вывода, не EOF.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// repeatingReader бесконечно повторяет chunk и считает прочитанные байты.
type repeatingReader struct {
	chunk string
	at    int
	read  int
}

func (r *repeatingReader) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		c := copy(p[n:], r.chunk[r.at:])
		n += c
		r.at = (r.at + c) % len(r.chunk)
	}
	r.read += n
	return n, nil
}

// FEATURE: HEAD repository проекта из объявления smart HTTP (#228).
// SPEC: Разбор читает pkt-lines объявления ссылок upload-pack протокола v0:
// строку service, необязательные метаданные до flush, необязательную строку
// "version 1" и первую ссылку. Существующий HEAD — первая ссылка; её объект —
// commit, а capability symref обязана назвать ветку. Остаток объявления не
// читается, чтение останавливается на пределе байт. Каждый дефект — один
// фиксированный отказ.
// IMPLEMENTATION: parseHeadAdvertisement, только stdlib.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestParseHeadAdvertisement(t *testing.T) {
	head := headTestCommit + " HEAD"
	header := pktLine("# service=git-upload-pack\n") + "0000"
	first := pktLine(head + "\x00" + headTestCaps + "\n")
	for _, tc := range []struct{ name, body string }{
		{"advertisement", headAdvertisement(head, headTestCaps)},
		{"service line without LF", pktLine("# service=git-upload-pack") + "0000" + first + "0000"},
		{"uppercase length", strings.ToUpper(pktLine("# service=git-upload-pack\n")[:4]) + "# service=git-upload-pack\n0000" + first},
		{"metadata before flush", pktLine("# service=git-upload-pack\n") + pktLine("# server metadata\n") + "0000" + first},
		{"protocol version 1", header + pktLine("version 1\n") + first},
		{"ref line without LF", header + pktLine(head+"\x00"+headTestCaps)},
		{"symref first", header + pktLine(head+"\x00symref=HEAD:refs/heads/trunk\n")},
		{"rest of the advertisement unread", header + first + "not a pkt-line"},
	} {
		commit, err := parseHeadAdvertisement(strings.NewReader(tc.body), gitHeadLimit)
		if err != nil || commit != headTestCommit {
			t.Errorf("%s: %q %v", tc.name, commit, err)
		}
	}
	if commit, err := parseHeadAdvertisement(io.MultiReader(strings.NewReader(header+first), failingReader{}), gitHeadLimit); err != nil || commit != headTestCommit {
		t.Errorf("read past HEAD: %q %v", commit, err)
	}

	sha256 := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"empty body", "", errGitHeadMalformed},
		{"not pkt-lines", "<html>login</html>", errGitHeadMalformed},
		{"non-hex length", "00zz# service=git-upload-pack\n", errGitHeadMalformed},
		{"signed length", "+01e# service=git-upload-pack\n", errGitHeadMalformed},
		{"length below the header", "0003", errGitHeadMalformed},
		{"delimiter packet", "0001", errGitHeadMalformed},
		{"length above the maximum", "fff1", errGitHeadMalformed},
		{"flush first", "0000", errGitHeadMalformed},
		{"receive-pack service", pktLine("# service=git-receive-pack\n") + "0000" + first, errGitHeadMalformed},
		{"no service line", first + "0000", errGitHeadMalformed},
		{"truncated header", "00", errGitHeadMalformed},
		{"truncated payload", "0040# service", errGitHeadMalformed},
		{"no flush after the service line", pktLine("# service=git-upload-pack\n"), errGitHeadMalformed},
		{"truncated before refs", header, errGitHeadMalformed},
		{"protocol version 2", header + pktLine("version 2\n") + pktLine("ls-refs\n") + "0000", errGitHeadMalformed},
		{"ref line without a name", header + pktLine(headTestCommit+"\x00"+headTestCaps+"\n"), errGitHeadMalformed},
		{"uppercase object name", header + pktLine(strings.ToUpper(headTestCommit)+" HEAD\x00"+headTestCaps+"\n"), errGitHeadMalformed},
		{"short object name", header + pktLine(headTestCommit[:39]+" HEAD\x00"+headTestCaps+"\n"), errGitHeadMalformed},
		{"zero object name", header + pktLine(strings.Repeat("0", 40)+" HEAD\x00"+headTestCaps+"\n"), errGitHeadMalformed},
		{"two HEAD symrefs", header + pktLine(head+"\x00symref=HEAD:refs/heads/trunk symref=HEAD:refs/heads/main\n"), errGitHeadMalformed},
		{"no refs", header + "0000", errGitHeadMissing},
		{"empty repository", header + pktLine(strings.Repeat("0", 40)+" capabilities^{}\x00"+headTestCaps+"\n") + "0000", errGitHeadMissing},
		{"HEAD not advertised", header + pktLine(headTestCommit+" refs/heads/trunk\x00"+headTestCaps+"\n") + "0000", errGitHeadMissing},
		{"detached HEAD", header + pktLine(head+"\x00multi_ack agent=git/2.43.0\n"), errGitHeadDetached},
		{"no capabilities", header + pktLine(head+"\n"), errGitHeadDetached},
		{"HEAD outside branches", header + pktLine(head+"\x00symref=HEAD:refs/tags/v1\n"), errGitHeadDetached},
		{"empty branch name", header + pktLine(head+"\x00symref=HEAD:refs/heads/\n"), errGitHeadDetached},
		{"SHA-256 repository", header + pktLine(sha256+" HEAD\x00symref=HEAD:refs/heads/trunk object-format=sha256\n"), errGitHeadNotSHA1},
		{"first ref past the limit", header + pktLine(head+"\x00"+headTestCaps+" "+strings.Repeat("x", 65516-len(head+"\x00"+headTestCaps+" \n"))+"\n"), errGitHeadTooLarge},
	} {
		commit, err := parseHeadAdvertisement(strings.NewReader(tc.body), gitHeadLimit)
		if commit != "" || !errors.Is(err, tc.want) {
			t.Errorf("%s: %q %v, want %v", tc.name, commit, err, tc.want)
		}
	}
	if _, err := parseHeadAdvertisement(io.MultiReader(strings.NewReader(header), failingReader{}), gitHeadLimit); !errors.Is(err, errGitHeadFailed) {
		t.Errorf("read error: %v", err)
	}
}

// SPEC: Бесконечные метаданные до ссылок не доводят: разбор отказывает,
// исчерпав предел, и не читает больше него.
func TestParseHeadAdvertisementBoundsReading(t *testing.T) {
	endless := &repeatingReader{chunk: pktLine(strings.Repeat("m", 996))}
	body := io.MultiReader(strings.NewReader(pktLine("# service=git-upload-pack\n")), endless)
	if _, err := parseHeadAdvertisement(body, gitHeadLimit); !errors.Is(err, errGitHeadTooLarge) {
		t.Fatalf("endless metadata: %v", err)
	}
	if endless.read > gitHeadLimit {
		t.Fatalf("read %d bytes past the %d byte limit", endless.read, gitHeadLimit)
	}
}
