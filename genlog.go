// genlog.go — 生成记录环形缓冲（内存，最近 200 条），供面板运行总览与记录页展示。
package main

import (
	"sync"
	"time"
)

const genLogCap = 200

// GenRecord 一次生图/测试的记录。
type GenRecord struct {
	Time      string `json:"time"`
	Kind      string `json:"kind"`     // generate / test
	Endpoint  string `json:"endpoint"` // /ai/generate-image | /admin/test
	Model     string `json:"model"`
	Prompt    string `json:"prompt"` // 截断展示
	Size      string `json:"size"`
	Via       string `json:"via"` // images / chat / images→chat兜底 / -
	OK        bool   `json:"ok"`
	Status    int    `json:"status"` // 返回给调用方的 HTTP 状态
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

type genLogStore struct {
	mu      sync.Mutex
	records []GenRecord
	success int
	fail    int
}

var genLog = &genLogStore{}

func (g *genLogStore) Add(r GenRecord) {
	r.Time = time.Now().Format("01-02 15:04:05")
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.OK {
		g.success++
	} else {
		g.fail++
	}
	g.records = append(g.records, r)
	if len(g.records) > genLogCap {
		g.records = g.records[len(g.records)-genLogCap:]
	}
}

// Snapshot 返回最新在前的前 limit 条。
func (g *genLogStore) Snapshot(limit int) []GenRecord {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := len(g.records)
	if limit <= 0 || limit > n {
		limit = n
	}
	out := make([]GenRecord, 0, limit)
	for i := n - 1; i >= n-limit; i-- {
		out = append(out, g.records[i])
	}
	return out
}

func (g *genLogStore) Counters() (success, fail int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.success, g.fail
}

func (g *genLogStore) Clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.records = nil
}
