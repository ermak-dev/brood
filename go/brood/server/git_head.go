package server

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"brood.dev/go/brood/app"
)

// FEATURE: Сервер разрешает ветку проекта в commit при запуске роя (#228).
// SPEC: Первый запуск без commit пиннит commit, на который сейчас указывает
// HEAD repository проекта (ветка по умолчанию). Сервер спрашивает его сам —
// тем же Git-доступом и транспортом, что у шлюза (#146, #173), — вне
// транзакции записи и до неё. Любой отказ — с причиной из фиксированного
// набора и без записи; адрес upstream, тело ответа и credential наружу и в
// журнал не попадают.
// IMPLEMENTATION: один GET info/refs?service=git-upload-pack протокола v0
// через транспорт шлюза (проверка адреса при dial, без прокси окружения и
// редиректов) и разбор pkt-line на stdlib. HEAD — первая ссылка объявления
// (gitprotocol-pack: «If HEAD is a valid ref, HEAD MUST appear as the first
// advertised ref»), поэтому читается только начало ответа, а не весь список
// ссылок: у репозитория с тысячами веток и pull-ссылок он велик. Протокол v2
// потребовал бы второго запроса ls-refs ради того же ответа. ls-remote
// подпроцессом — это git в образе сервера (scratch) и credential в окружении
// или аргументах дочернего процесса.
// RESULT: IMPLEMENTED — схема не меняется.
// STATUS: DONE

const (
	// gitHeadLimit — байты объявления, которые читаются до HEAD: строка
	// service, метаданные и первая ссылка с capabilities. Настоящая первая
	// строка — сотни байт; предел не даёт upstream кормить сервер метаданными.
	gitHeadLimit = 64 << 10
	// gitHeadTimeout — весь запрос: соединение, TLS, заголовки и чтение.
	gitHeadTimeout = 30 * time.Second
	// gitPktMax — наибольшая pkt-line вместе с четырьмя байтами длины.
	gitPktMax = 65520
	// gitAdvertisementType — Content-Type ответа smart HTTP; у dumb HTTP он
	// другой, его объявление HEAD не называет.
	gitAdvertisementType = "application/x-git-upload-pack-advertisement"
)

// gitHeadError — отказ разрешения ветки: статус ответа и фиксированная
// причина. Ни адреса upstream, ни его тела, ни ошибок транспорта в ней нет.
type gitHeadError struct {
	status int
	reason string
}

func (e *gitHeadError) Error() string { return "cannot resolve the project branch: " + e.reason }

var (
	errGitHeadNoAccess  = &gitHeadError{http.StatusUnprocessableEntity, "the project repository has no server Git access; bind a project Git credential or name the commit"}
	errGitHeadFailed    = &gitHeadError{http.StatusBadGateway, "Git upstream request failed"}
	errGitHeadAddress   = &gitHeadError{http.StatusBadGateway, "Git upstream address denied"}
	errGitHeadTimeout   = &gitHeadError{http.StatusGatewayTimeout, "Git upstream request timed out"}
	errGitHeadDenied    = &gitHeadError{http.StatusBadGateway, "Git upstream denied access"}
	errGitHeadNotFound  = &gitHeadError{http.StatusBadGateway, "Git upstream repository not found"}
	errGitHeadRedirect  = &gitHeadError{http.StatusBadGateway, "Git upstream redirect denied"}
	errGitHeadMalformed = &gitHeadError{http.StatusBadGateway, "malformed Git ref advertisement"}
	errGitHeadTooLarge  = &gitHeadError{http.StatusBadGateway, "Git ref advertisement too large"}
	errGitHeadMissing   = &gitHeadError{http.StatusUnprocessableEntity, "the repository has no default branch"}
	errGitHeadDetached  = &gitHeadError{http.StatusUnprocessableEntity, "the repository HEAD is not a branch"}
	errGitHeadNotSHA1   = &gitHeadError{http.StatusUnprocessableEntity, "the repository HEAD is not a SHA-1 commit"}
)

// startCommit — commit первого запуска, которому тело commit не назвало:
// HEAD repository проекта, разрешённый до транзакции записи. Пустой ответ
// без отказа — разрешать незачем: повторный запуск идёт на закреплённом
// источнике, а неактивный рой, отказ в праве управлять и проект без
// repository StartSwarm отвергнет со своей причиной. Право проверяется до
// сети, чтобы зритель роя не гонял upstream с credential проекта.
// Возвращает и repository, из которого commit разрешён: транзакция сверит
// его с repository проекта (store.SwarmStart.Origin).
func (s *Server) startCommit(ctx context.Context, swarmID, userID string) (origin, commit string, err error) {
	sw, err := s.store.GetSwarm(ctx, swarmID)
	if err != nil || sw.Graph != nil || sw.State != "active" {
		return "", "", err
	}
	manage, err := s.app.Can(userID, app.ActionManageSwarm, app.SwarmRes(swarmID))
	if err != nil || !manage {
		return "", "", err
	}
	access, err := s.projectGitAccess(ctx, sw.ProjectID)
	if err != nil || access.repository == "" {
		return "", "", err
	}
	if !access.gateway {
		return "", "", errGitHeadNoAccess
	}
	commit, err = s.git.resolveHead(ctx, access)
	if err != nil {
		return "", "", err
	}
	return access.repository, commit, nil
}

// resolveHead спрашивает объявление ссылок repository и возвращает commit
// его HEAD. Запрос несёт только фиксированный User-Agent и Basic credential
// доступа; транспорт шлюза редиректы не выполняет, ответ 3xx — отказ.
func (g *gitGateway) resolveHead(ctx context.Context, access gitAccess) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.headTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, access.repository+"/info/refs?service=git-upload-pack", http.NoBody)
	if err != nil {
		return "", errGitHeadFailed
	}
	req.Header.Set("User-Agent", gitGatewayUserAgent)
	req.Header.Set("Pragma", "no-cache")
	if access.credential.configured() {
		req.SetBasicAuth(access.credential.username, access.credential.token)
	}
	resp, err := g.transport.RoundTrip(req)
	if err != nil {
		return "", headRequestError(ctx, err)
	}
	defer resp.Body.Close()
	if err := headResponseError(resp); err != nil {
		return "", err
	}
	commit, err := parseHeadAdvertisement(resp.Body, gitHeadLimit)
	if err != nil && ctx.Err() != nil {
		return "", headRequestError(ctx, ctx.Err())
	}
	return commit, err
}

// headRequestError сводит ошибку транспорта к фиксированной причине: текст
// ошибки содержит адрес upstream и наружу не идёт.
func headRequestError(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return errGitHeadTimeout
	case errors.Is(err, errGitAddressDenied):
		return errGitHeadAddress
	default:
		return errGitHeadFailed
	}
}

// headResponseError — отказ по статусу и типу ответа до чтения тела.
func headResponseError(resp *http.Response) error {
	switch code := resp.StatusCode; {
	case code >= 300 && code < 400:
		return errGitHeadRedirect
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return errGitHeadDenied
	case code == http.StatusNotFound:
		return errGitHeadNotFound
	case code != http.StatusOK:
		return errGitHeadFailed
	}
	if media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || media != gitAdvertisementType {
		return errGitHeadMalformed
	}
	return nil
}

// parseHeadAdvertisement читает объявление ссылок upload-pack протокола v0
// (gitprotocol-http, «Smart Clients»): строку service, метаданные до flush,
// необязательную строку "version 1" и первую ссылку. Commit — имя объекта
// HEAD, его capability symref обязана назвать ветку. Остаток ответа не
// читается; больше limit байт не читается никогда.
func parseHeadAdvertisement(body io.Reader, limit int) (string, error) {
	pkt := &pktReader{r: body, budget: limit}
	line, flush, err := pkt.next()
	if err != nil {
		return "", err
	}
	if flush || strings.TrimSuffix(line, "\n") != "# service=git-upload-pack" {
		return "", errGitHeadMalformed
	}
	// Git-клиент пропускает метаданные после строки service до flush.
	for !flush {
		if _, flush, err = pkt.next(); err != nil {
			return "", err
		}
	}
	if line, flush, err = pkt.next(); err != nil {
		return "", err
	}
	if !flush && strings.TrimSuffix(line, "\n") == "version 1" {
		if line, flush, err = pkt.next(); err != nil {
			return "", err
		}
	}
	if flush {
		return "", errGitHeadMissing
	}
	return headCommit(strings.TrimSuffix(line, "\n"))
}

// headCommit разбирает первую ссылку "<oid> <name>\0<capabilities>". Пустой
// репозиторий объявляет вместо ссылок "capabilities^{}" с нулевым oid, а
// HEAD на несуществующей ветке не объявляется вовсе.
func headCommit(line string) (string, error) {
	ref, capabilities, _ := strings.Cut(line, "\x00")
	oid, name, ok := strings.Cut(ref, " ")
	if !ok || !gitObjectName(oid) {
		return "", errGitHeadMalformed
	}
	if name != "HEAD" {
		return "", errGitHeadMissing
	}
	if strings.Trim(oid, "0") == "" {
		return "", errGitHeadMalformed
	}
	branch := ""
	for _, capability := range strings.Split(capabilities, " ") {
		target, ok := strings.CutPrefix(capability, "symref=HEAD:")
		if !ok {
			continue
		}
		if branch != "" {
			return "", errGitHeadMalformed
		}
		branch = target
	}
	if name, ok := strings.CutPrefix(branch, "refs/heads/"); !ok || name == "" {
		return "", errGitHeadDetached
	}
	// Источник роя пиннит только SHA-1 (store.canonicalPipelineSource).
	if len(oid) != 40 {
		return "", errGitHeadNotSHA1
	}
	return oid, nil
}

// gitObjectName — имя объекта SHA-1 или SHA-256 в нижнем регистре, как его
// печатает Git.
func gitObjectName(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for i := 0; i < len(oid); i++ {
		if c := oid[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// pktReader читает pkt-lines, расходуя бюджет байт до чтения: длина строки
// известна из её заголовка, поэтому перерасхода нет.
type pktReader struct {
	r      io.Reader
	budget int
}

// next — следующая pkt-line: flush или данные. Delim и response-end бывают
// только в протоколе v2, здесь это повреждение.
func (p *pktReader) next() (line string, flush bool, err error) {
	header, err := p.read(4)
	if err != nil {
		return "", false, err
	}
	length, err := strconv.ParseUint(string(header), 16, 16)
	switch {
	case err != nil:
		return "", false, errGitHeadMalformed
	case length == 0:
		return "", true, nil
	case length < 4 || length > gitPktMax:
		return "", false, errGitHeadMalformed
	}
	payload, err := p.read(int(length) - 4)
	return string(payload), false, err
}

func (p *pktReader) read(n int) ([]byte, error) {
	if n > p.budget {
		return nil, errGitHeadTooLarge
	}
	p.budget -= n
	buf := make([]byte, n)
	if _, err := io.ReadFull(p.r, buf); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errGitHeadMalformed
		}
		return nil, errGitHeadFailed
	}
	return buf, nil
}
