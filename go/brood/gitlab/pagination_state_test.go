package gitlab

// BUG: Поздний total и объявленный last допускают неполные свидетельства CI.
// HYPOTHESIS: Проверяется только текущая страница, без истории размеров страниц.
// SOLUTION: Один валидатор хранит размеры всех страниц и сверяет их с last/total,
// включая поздние ограничения; объявленные границы требуют согласованного next.
// RESULT: FIXED - Четыре исходных TLS-пробы больше не дают ложный success;
// неполные списки jobs, triggers и MR pipelines возвращают nil с отказом.
// STATUS: FIXED

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

type paginationFixturePage struct {
	count      int
	next       int
	lastLink   int
	totalPages string
	total      string
}

func (p paginationFixturePage) write(w http.ResponseWriter, r *http.Request) {
	if p.next > 0 {
		w.Header().Set("X-Next-Page", strconv.Itoa(p.next))
	}
	if p.lastLink > 0 {
		query := r.URL.Query()
		query.Set("page", strconv.Itoa(p.lastLink))
		w.Header().Set("Link", "<https://"+r.Host+r.URL.Path+"?"+query.Encode()+">; rel=last")
	}
	if p.totalPages != "" {
		w.Header().Set("X-Total-Pages", p.totalPages)
	}
	if p.total != "" {
		w.Header().Set("X-Total", p.total)
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	rows := make([]Job, p.count)
	for i := range rows {
		rows[i] = Job{ID: int64((page-1)*100 + i + 1), Status: "success", Pipeline: Pipeline{ID: 90, ProjectID: 42, SHA: "abc"}}
	}
	_ = json.NewEncoder(w).Encode(rows)
}

func TestCrossPageCompletenessRefusesPartialEvidence(t *testing.T) {
	for _, tt := range []struct {
		name  string
		pages []paginationFixturePage
	}{
		{"late total", []paginationFixturePage{{count: 1, next: 2}, {count: 1, total: "101"}}},
		{"empty declared terminal", []paginationFixturePage{{count: 100, next: 2, totalPages: "2"}, {count: 0, totalPages: "2"}}},
		{"short nonterminal total pages", []paginationFixturePage{{count: 1, next: 2, totalPages: "2"}, {count: 1, totalPages: "2"}}},
		{"short nonterminal last link", []paginationFixturePage{{count: 1, next: 2, lastLink: 2}, {count: 1, lastLink: 2}}},
		{"late total pages", []paginationFixturePage{{count: 1, next: 2}, {count: 1, totalPages: "2"}}},
		{"late last link", []paginationFixturePage{{count: 1, next: 2}, {count: 1, lastLink: 2}}},
		{"late total checks middle page", []paginationFixturePage{{count: 100, next: 2}, {count: 1, next: 3}, {count: 1, total: "201"}}},
		{"late nonterminal last checks history", []paginationFixturePage{{count: 1, next: 2}, {count: 100, next: 3, totalPages: "3"}, {count: 1}}},
		{"late empty terminal link", []paginationFixturePage{{count: 100, next: 2}, {count: 0, lastLink: 2}}},
		{"remembered empty terminal", []paginationFixturePage{{count: 100, next: 2, lastLink: 2}, {count: 0}}},
		{"remembered last needs next", []paginationFixturePage{{count: 100, next: 2, totalPages: "3"}, {count: 100}, {count: 1}}},
		{"changed last", []paginationFixturePage{{count: 100, next: 2, totalPages: "3"}, {count: 1, lastLink: 2}}},
		{"changed total", []paginationFixturePage{{count: 100, next: 2, total: "201"}, {count: 100, next: 3, total: "202"}, {count: 2}}},
		{"late total contradicts remembered last", []paginationFixturePage{{count: 100, next: 2, lastLink: 3}, {count: 1, total: "101"}}},
		{"late total contradicts terminal count", []paginationFixturePage{{count: 100, next: 2}, {count: 1, total: "102"}}},
	} {
		for _, endpoint := range []string{"jobs", "trigger_jobs"} {
			t.Run(tt.name+"/"+endpoint, func(t *testing.T) {
				calls := 0
				c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
					if !strings.HasSuffix(r.URL.Path, "/"+endpoint) {
						fmt.Fprint(w, `[]`)
						return
					}
					calls++
					page, _ := strconv.Atoi(r.URL.Query().Get("page"))
					if page < 1 || page > len(tt.pages) {
						t.Error("unexpected page", page)
						http.Error(w, "unexpected page", http.StatusInternalServerError)
						return
					}
					tt.pages[page-1].write(w, r)
				})
				jobs, err := c.Jobs(context.Background(), 90)
				if err == nil || jobs != nil {
					e := AssessCI(Pipeline{ID: 90, ProjectID: 42, SHA: "abc", Status: "success"}, jobs)
					t.Fatalf("contradictory pagination accepted: requests=%d rows=%d err=%v evidence=%+v", calls, len(jobs), err, e)
				}
				checkError(t, err, Response)
			})
		}
	}
}

func TestCrossPageCompletenessAcceptsConsistentEvidence(t *testing.T) {
	for _, tt := range []struct {
		name  string
		pages []paginationFixturePage
		want  int
	}{
		{"late total", []paginationFixturePage{{count: 100, next: 2}, {count: 1, total: "101"}}, 101},
		{"late total pages", []paginationFixturePage{{count: 100, next: 2}, {count: 1, totalPages: "2"}}, 101},
		{"late last link", []paginationFixturePage{{count: 100, next: 2}, {count: 1, lastLink: 2}}, 101},
		{"late full terminal", []paginationFixturePage{{count: 100, next: 2}, {count: 100, total: "200"}}, 200},
		{"remembered total", []paginationFixturePage{{count: 100, next: 2, total: "101"}, {count: 1}}, 101},
		{"remembered full terminal", []paginationFixturePage{{count: 100, next: 2, lastLink: 2}, {count: 100}}, 200},
		{"late nonterminal total", []paginationFixturePage{{count: 100, next: 2}, {count: 100, next: 3, total: "201"}, {count: 1}}, 201},
		{"unannounced empty probe", []paginationFixturePage{{count: 100}, {count: 0}}, 100},
		{"short next without totals", []paginationFixturePage{{count: 1, next: 2}, {count: 1}}, 2},
		{"zero total", []paginationFixturePage{{count: 0, total: "0"}}, 0},
		{"zero pages", []paginationFixturePage{{count: 0, totalPages: "0"}}, 0},
		{"empty first last link", []paginationFixturePage{{count: 0, lastLink: 1}}, 0},
		{"short single page", []paginationFixturePage{{count: 1, totalPages: "1"}}, 1},
	} {
		for _, endpoint := range []string{"jobs", "trigger_jobs"} {
			t.Run(tt.name+"/"+endpoint, func(t *testing.T) {
				calls := 0
				c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
					if !strings.HasSuffix(r.URL.Path, "/"+endpoint) {
						fmt.Fprint(w, `[]`)
						return
					}
					calls++
					page, _ := strconv.Atoi(r.URL.Query().Get("page"))
					if page < 1 || page > len(tt.pages) {
						t.Error("unexpected page", page)
						http.Error(w, "unexpected page", http.StatusInternalServerError)
						return
					}
					tt.pages[page-1].write(w, r)
				})
				jobs, err := c.Jobs(context.Background(), 90)
				if err != nil || jobs == nil || len(jobs) != tt.want || calls != len(tt.pages) {
					t.Fatalf("consistent evidence refused or over-probed: requests=%d rows=%d err=%v", calls, len(jobs), err)
				}
			})
		}
	}
}

func TestMRPipelineLateTotalRefusesBeforeHydration(t *testing.T) {
	lists, hydrated := 0, 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/merge_requests/8"):
			fmt.Fprint(w, `{"id":80,"iid":8,"project_id":42,"source_project_id":42,"target_project_id":42}`)
		case strings.HasSuffix(r.URL.Path, "/merge_requests/8/pipelines"):
			lists++
			p := paginationFixturePage{count: 1, next: 2}
			if r.URL.Query().Get("page") == "2" {
				p = paginationFixturePage{count: 1, total: "101"}
			}
			p.write(w, r)
		default:
			hydrated++
			id, _ := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			fmt.Fprintf(w, `{"id":%d,"project_id":42}`, id)
		}
	})
	pipelines, err := c.MergeRequestPipelines(context.Background(), 8)
	checkError(t, err, Response)
	if pipelines != nil || lists != 2 || hydrated != 0 {
		t.Fatalf("incomplete references hydrated or retained: lists=%d hydrated=%d rows=%d", lists, hydrated, len(pipelines))
	}
}
