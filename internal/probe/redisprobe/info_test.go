package redisprobe

import (
	"math"
	"testing"
	"time"

	"github.com/danilopopovikj/wassup/internal/model"
)

const infoFixture = `# Server
redis_version:7.2.4
redis_mode:standalone

# Clients
connected_clients:12
blocked_clients:0

# Memory
used_memory:2147483648
used_memory_human:2.00G
total_system_memory:8589934592
maxmemory:2147483648
maxmemory_human:2.00G
maxmemory_policy:noeviction

# Stats
keyspace_hits:1000
keyspace_misses:9000
evicted_keys:52000

# Keyspace
db0:keys=200000,expires=16000,avg_ttl=1200
db1:keys=50,expires=0,avg_ttl=0
`

func TestParseInfo(t *testing.T) {
	info := ParseInfo(infoFixture)
	if got := info.Str("redis_version"); got != "7.2.4" {
		t.Errorf("redis_version = %q", got)
	}
	if v, ok := info.Num("connected_clients"); !ok || v != 12 {
		t.Errorf("connected_clients = %v %v", v, ok)
	}
	if _, ok := info.Num("redis_mode"); ok {
		t.Error("redis_mode should not parse as a number")
	}
	if _, ok := info.Num("missing"); ok {
		t.Error("missing field should not parse")
	}
	if len(info.Keyspace) != 2 {
		t.Fatalf("keyspace entries = %d", len(info.Keyspace))
	}
	if info.Keyspace[0].Name != "db0" || info.Keyspace[0].Keys != 200000 || info.Keyspace[0].Expires != 16000 || info.Keyspace[0].AvgTTL != 1200 {
		t.Errorf("db0 = %+v", info.Keyspace[0])
	}
	keys, expires := info.Keys()
	if keys != 200050 || expires != 16000 {
		t.Errorf("keys=%v expires=%v", keys, expires)
	}
}

func TestObserveFirstTickLifetimeRates(t *testing.T) {
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	m, conds, detail, cur := Observe(ParseInfo(infoFixture), nil, now)
	if m["mem_pct"] != 100 {
		t.Errorf("mem_pct = %v", m["mem_pct"])
	}
	if m["hit_rate"] != 10 {
		t.Errorf("hit_rate = %v, want lifetime 10", m["hit_rate"])
	}
	if _, ok := m["evictions"]; ok {
		t.Error("evictions should be omitted without a previous sample")
	}
	if m["clients"] != 12 {
		t.Errorf("clients = %v", m["clients"])
	}
	c, ok := model.HasCondition(conds, model.CondCacheFull)
	if !ok {
		t.Fatalf("CacheFull missing: %+v", conds)
	}
	if c.Ref != "policy/noeviction" {
		t.Errorf("ref = %q", c.Ref)
	}
	if detail["maxmemory"] != "2.00G" || detail["maxmemory_policy"] != "noeviction" || detail["redis_version"] != "7.2.4" {
		t.Errorf("detail = %+v", detail)
	}
	if detail["keys_without_ttl"] != float64(184050) {
		t.Errorf("keys_without_ttl = %v", detail["keys_without_ttl"])
	}
	if cur.Hits != 1000 || cur.Misses != 9000 || cur.Evicted != 52000 || !cur.At.Equal(now) {
		t.Errorf("sample = %+v", cur)
	}
}

func TestObserveDeltaRates(t *testing.T) {
	prevAt := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	prev := &Sample{At: prevAt, Hits: 900, Misses: 8700, Evicted: 51740}
	m, _, _, _ := Observe(ParseInfo(infoFixture), prev, prevAt.Add(3*time.Second))
	// 100 hits and 300 misses since the last tick -> 25%.
	if m["hit_rate"] != 25 {
		t.Errorf("hit_rate = %v, want 25", m["hit_rate"])
	}
	// 260 evictions in 3s -> 5200 per minute.
	if math.Abs(m["evictions"]-5200) > 1e-9 {
		t.Errorf("evictions = %v, want 5200", m["evictions"])
	}
}

func TestObserveNotFull(t *testing.T) {
	text := `used_memory:1000
maxmemory:0
total_system_memory:4000
maxmemory_policy:allkeys-lru
keyspace_hits:90
keyspace_misses:10
`
	m, conds, _, _ := Observe(ParseInfo(text), nil, time.Now())
	if m["mem_pct"] != 25 {
		t.Errorf("mem_pct = %v, want used/total", m["mem_pct"])
	}
	if len(conds) != 0 {
		t.Errorf("unexpected conditions %+v", conds)
	}
	// Full but evicting with a healthy hit rate is not CacheFull.
	full := `used_memory:1000
maxmemory:1000
maxmemory_policy:allkeys-lru
keyspace_hits:90
keyspace_misses:10
`
	_, conds, _, _ = Observe(ParseInfo(full), nil, time.Now())
	if len(conds) != 0 {
		t.Errorf("lru cache at 100%% with 90%% hits should not be CacheFull: %+v", conds)
	}
	// Full, evicting, and the hit rate collapsed: CacheFull.
	storm := `used_memory:1000
maxmemory:1000
maxmemory_policy:allkeys-lru
keyspace_hits:10
keyspace_misses:90
`
	_, conds, _, _ = Observe(ParseInfo(storm), nil, time.Now())
	if _, ok := model.HasCondition(conds, model.CondCacheFull); !ok {
		t.Errorf("want CacheFull on a miss storm: %+v", conds)
	}
}

func TestValidate(t *testing.T) {
	p := &InfoProbe{}
	if err := p.Validate(map[string]any{}); err == nil {
		t.Error("empty spec should fail")
	}
	if err := p.Validate(map[string]any{"addr": "cache:6379", "db": 2, "tls": true}); err != nil {
		t.Errorf("valid spec: %v", err)
	}
	if err := p.Validate(map[string]any{"url": "redis://cache:6379/1"}); err != nil {
		t.Errorf("valid url: %v", err)
	}
	if err := p.Validate(map[string]any{"url": "http://nope"}); err == nil {
		t.Error("bad scheme should fail")
	}
	if err := p.Validate(map[string]any{"addr": "cache:6379", "db": "two"}); err == nil {
		t.Error("non-numeric db should fail")
	}
	l := &ListProbe{}
	if err := l.Validate(map[string]any{"addr": "cache:6379"}); err == nil {
		t.Error("list without key should fail")
	}
}

func TestOptionsReadsPasswordFromEnv(t *testing.T) {
	t.Setenv("WASSUP_TEST_REDIS_PW", "s3cret")
	opt, err := options(map[string]any{"addr": "cache:6379", "password_env": "WASSUP_TEST_REDIS_PW", "db": 3, "tls": true}, "url")
	if err != nil {
		t.Fatal(err)
	}
	if opt.Password != "s3cret" || opt.DB != 3 || opt.TLSConfig == nil || opt.Addr != "cache:6379" {
		t.Errorf("options = %+v", opt)
	}
	if _, err := options(map[string]any{"addr": "cache:6379", "password_env": "WASSUP_TEST_REDIS_PW_MISSING"}, "url"); err == nil {
		t.Error("missing env var should fail")
	}
}
