package gitlab

// BUG: Сервер не может проверить create target тем же контрактом до резерва.
// HYPOTHESIS: Чистая проверка ветки транспорта недоступна вызывающему слою.
// SOLUTION: Экспортирован существующий predicate без изменения синтаксиса.
// RESULT: FIXED — Общая чистая проверка доступна до transport invocation.
// STATUS: FIXED

import (
	"strings"
	"testing"
)

func TestValidCreateBranch(t *testing.T) {
	for _, branch := range []string{"main", "release/hotfix", "topic_1.2-3", "brood/actor/1/work", strings.Repeat("x", MaxCreateBranchBytes)} {
		if !ValidCreateBranch(branch) {
			t.Errorf("supported create branch refused: %q", branch)
		}
	}
	for _, branch := range []string{"release+hotfix", "", "HEAD", "-option", "refs/heads/main", strings.Repeat("a", 40), "a//b", "a..b", "a.lock", "a/.b", "a./b", "a b", "a\x00b", strings.Repeat("x", MaxCreateBranchBytes+1)} {
		if ValidCreateBranch(branch) {
			t.Errorf("unsupported create branch accepted: %q", branch)
		}
	}
}
