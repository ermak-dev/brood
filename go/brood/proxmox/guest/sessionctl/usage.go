//go:build linux

package sessionctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"time"

	"brood.dev/go/brood/proxmox/relay"
)

type usageReport struct {
	Days        int                 `json:"days"`
	Input       int64               `json:"input"`
	Output      int64               `json:"output"`
	CacheWrite  int64               `json:"cache_write"`
	CacheRead   int64               `json:"cache_read"`
	Attribution string              `json:"attribution"`
	Partial     bool                `json:"partial"`
	Files       int                 `json:"files"`
	PerDay      map[string][4]int64 `json:"per_day"`
	Scope       string              `json:"scope"`
}

func (e *engine) usage(ctx context.Context, days int) relay.SessionResponse {
	now := e.now()
	y, m, d := now.Date()
	since := time.Date(y, m, d, 0, 0, 0, 0, now.Location()).AddDate(0, 0, 1-days)
	report := usageReport{Days: days, Attribution: "unknown", PerDay: map[string][4]int64{}, Scope: "this guest project; bounded transcript scan"}
	seen := map[string]bool{}
	remaining := int64(32 * 1024 * 1024)
	stop := false
	var walk func(string, int)
	walk = func(path string, depth int) {
		if stop {
			return
		}
		if ctx.Err() != nil {
			report.Partial = true
			stop = true
			return
		}
		dir, err := openSafe(path, true)
		if err != nil {
			report.Partial = true
			return
		}
		defer dir.Close()
		entries, err := dir.ReadDir(513)
		if err != nil && err != io.EOF {
			report.Partial = true
			return
		}
		if len(entries) > 512 {
			report.Partial = true
			entries = entries[:512]
		}
		for _, entry := range entries {
			if stop {
				return
			}
			if !entry.Type().IsRegular() && !entry.IsDir() {
				report.Partial = true
				continue
			}
			p := filepath.Join(path, entry.Name())
			if entry.IsDir() {
				if depth < 3 {
					walk(p, depth+1)
				} else {
					report.Partial = true
				}
				continue
			}
			if !strings.HasSuffix(entry.Name(), ".jsonl") {
				continue
			}
			if report.Files >= 512 || remaining <= 0 || len(seen) >= 32768 {
				report.Partial = true
				stop = true
				return
			}
			f, err := openSafe(p, false)
			if err != nil {
				report.Partial = true
				continue
			}
			report.Files++
			size := min(remaining, int64(8*1024*1024))
			reader := io.LimitReader(f, size+1)
			scan := bufio.NewScanner(reader)
			scan.Buffer(make([]byte, 4096), 64*1024)
			scan.Split(func(data []byte, atEOF bool) (int, []byte, error) {
				if end := bytes.IndexByte(data, '\n'); end >= 0 {
					return end + 1, data[:end+1], nil
				}
				if atEOF && len(data) > 0 {
					return len(data), data, nil
				}
				return 0, nil, nil
			})
			read := int64(0)
			for scan.Scan() {
				n := int64(len(scan.Bytes()))
				read += n
				if !bytes.HasSuffix(scan.Bytes(), []byte{'\n'}) {
					report.Partial = true
					break
				}
				if read > size || len(seen) >= 32768 || ctx.Err() != nil {
					report.Partial = true
					stop = true
					break
				}
				var row struct {
					Type      string `json:"type"`
					Timestamp string `json:"timestamp"`
					Message   struct {
						ID    string `json:"id"`
						Usage *struct {
							Input      int64 `json:"input_tokens"`
							Output     int64 `json:"output_tokens"`
							CacheWrite int64 `json:"cache_creation_input_tokens"`
							CacheRead  int64 `json:"cache_read_input_tokens"`
						} `json:"usage"`
					} `json:"message"`
				}
				if json.Unmarshal(scan.Bytes(), &row) != nil {
					report.Partial = true
					continue
				}
				if row.Type != "assistant" || row.Message.Usage == nil {
					continue
				}
				ts, err := time.Parse(time.RFC3339Nano, row.Timestamp)
				if err != nil {
					report.Partial = true
					continue
				}
				if ts.Before(since) || ts.After(now) {
					continue
				}
				id := row.Message.ID
				if id == "" || len(id) > 512 {
					report.Partial = true
					continue
				}
				if seen[id] {
					continue
				}
				seen[id] = true
				u := row.Message.Usage
				values := [4]int64{u.Input, u.Output, u.CacheWrite, u.CacheRead}
				valid := true
				for _, v := range values {
					if v < 0 || v > 1_000_000_000_000 {
						valid = false
					}
				}
				if !valid {
					report.Partial = true
					continue
				}
				day := ts.In(now.Location()).Format("2006-01-02")
				sum := report.PerDay[day]
				for i, v := range values {
					sum[i] += v
				}
				report.PerDay[day] = sum
				report.Input += u.Input
				report.Output += u.Output
				report.CacheWrite += u.CacheWrite
				report.CacheRead += u.CacheRead
			}
			if scan.Err() != nil {
				report.Partial = true
			}
			remaining -= min(read, size)
			if st, err := f.Stat(); err != nil || st.Size() > size {
				report.Partial = true
			}
			f.Close()
		}
	}
	walk(filepath.Join(e.config, "projects", "-workspace-project"), 0)
	return success(report)
}
