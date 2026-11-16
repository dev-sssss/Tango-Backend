package server

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Metrics is a small, dependency-free Prometheus registry. Series are
// identified by name plus sorted labels.
type Metrics struct {
	ns     string
	mu     sync.RWMutex
	series map[string]*series
}

type series struct {
	kind   string // counter | gauge | summary
	name   string
	labels string
	value  atomic.Int64 // counters (integers) and summary count
	fvalue atomic.Uint64
	sum    atomic.Int64
}

func NewMetrics(namespace string) *Metrics {
	return &Metrics{ns: namespace, series: map[string]*series{}}
}

func sanitizeMetricName(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r == '_' || r == ':' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (i > 0 && r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

func labelString(tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		v := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(tags[k])
		parts[i] = fmt.Sprintf(`%s="%s"`, sanitizeMetricName(k), v)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func (m *Metrics) get(kind, name string, tags map[string]string) *series {
	full := sanitizeMetricName(name)
	if m.ns != "" {
		full = m.ns + "_" + full
	}
	ls := labelString(tags)
	key := kind + "|" + full + ls
	m.mu.RLock()
	s, ok := m.series[key]
	m.mu.RUnlock()
	if ok {
		return s
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok = m.series[key]; ok {
		return s
	}
	s = &series{kind: kind, name: full, labels: ls}
	m.series[key] = s
	return s
}

func (m *Metrics) CounterAdd(name string, tags map[string]string, delta int64) {
	m.get("counter", name, tags).value.Add(delta)
}

func (m *Metrics) GaugeSet(name string, tags map[string]string, v float64) {
	m.get("gauge", name, tags).fvalue.Store(math.Float64bits(v))
}

func (m *Metrics) GaugeAdd(name string, tags map[string]string, delta float64) {
	s := m.get("gauge", name, tags)
	for {
		old := s.fvalue.Load()
		nv := math.Float64bits(math.Float64frombits(old) + delta)
		if s.fvalue.CompareAndSwap(old, nv) {
			return
		}
	}
}

// TimerRecord records a duration in nanoseconds as a summary (_sum seconds, _count).
func (m *Metrics) TimerRecord(name string, tags map[string]string, nanos int64) {
	s := m.get("summary", name, tags)
	s.value.Add(1)
	s.sum.Add(nanos)
}

func (m *Metrics) Write(w io.Writer) {
	m.mu.RLock()
	list := make([]*series, 0, len(m.series))
	for _, s := range m.series {
		list = append(list, s)
	}
	m.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].name != list[j].name {
			return list[i].name < list[j].name
		}
		return list[i].labels < list[j].labels
	})
	typed := map[string]bool{}
	for _, s := range list {
		if !typed[s.name] {
			typed[s.name] = true
			t := s.kind
			fmt.Fprintf(w, "# TYPE %s %s\n", s.name, t)
		}
		switch s.kind {
		case "counter":
			fmt.Fprintf(w, "%s%s %d\n", s.name, s.labels, s.value.Load())
		case "gauge":
			fmt.Fprintf(w, "%s%s %g\n", s.name, s.labels, math.Float64frombits(s.fvalue.Load()))
		case "summary":
			fmt.Fprintf(w, "%s_sum%s %g\n", s.name, s.labels, float64(s.sum.Load())/1e9)
			fmt.Fprintf(w, "%s_count%s %d\n", s.name, s.labels, s.value.Load())
		}
	}
}

func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		m.Write(w)
	})
}
