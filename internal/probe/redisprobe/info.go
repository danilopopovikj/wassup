package redisprobe

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/danilopopovikj/wassup/internal/model"
	"github.com/danilopopovikj/wassup/internal/probe"
)

// Info is the parsed output of INFO: every "key:value" line plus the
// keyspace lines broken into per-database counters.
type Info struct {
	Fields   map[string]string
	Keyspace []KeyspaceDB
}

// KeyspaceDB is one "dbN:keys=..,expires=..,avg_ttl=.." line.
type KeyspaceDB struct {
	Name    string
	Keys    float64
	Expires float64
	AvgTTL  float64
}

// ParseInfo parses the text INFO returns. Section headers ("# Memory") and
// blank lines are skipped; keyspace lines are split into KeyspaceDB entries
// and also kept in Fields.
func ParseInfo(text string) Info {
	info := Info{Fields: map[string]string{}}
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		info.Fields[k] = v
		if strings.HasPrefix(k, "db") && strings.Contains(v, "keys=") {
			db := KeyspaceDB{Name: k}
			for _, part := range strings.Split(v, ",") {
				pk, pv, _ := strings.Cut(part, "=")
				f, _ := strconv.ParseFloat(pv, 64)
				switch pk {
				case "keys":
					db.Keys = f
				case "expires":
					db.Expires = f
				case "avg_ttl":
					db.AvgTTL = f
				}
			}
			info.Keyspace = append(info.Keyspace, db)
		}
	}
	return info
}

// Num returns a numeric field, or 0 and false when absent or not a number.
func (i Info) Num(key string) (float64, bool) {
	v, ok := i.Fields[key]
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// Str returns a string field or "".
func (i Info) Str(key string) string { return i.Fields[key] }

// Keys sums keys over every database; Expires sums keys with a TTL.
func (i Info) Keys() (keys, expires float64) {
	for _, db := range i.Keyspace {
		keys += db.Keys
		expires += db.Expires
	}
	return keys, expires
}

// Sample is what one tick remembers for the next: the lifetime counters
// hit_rate and evictions are differentiated against.
type Sample struct {
	At      time.Time
	Hits    float64
	Misses  float64
	Evicted float64
}

// cacheFullMemPct is the fill level at which a cache counts as full.
const cacheFullMemPct = 99.5

// Observe maps an INFO reading onto the cache metrics vocabulary. prev is
// the previous tick's sample (nil on the first tick): with it hit_rate is the
// hit rate since that tick and evictions the evicted keys per minute; without
// it hit_rate is the lifetime rate and evictions is omitted. The returned
// CacheFull condition has no Since; the probe stamps it.
func Observe(info Info, prev *Sample, now time.Time) (map[string]float64, []model.Condition, map[string]any, Sample) {
	m := map[string]float64{}
	detail := map[string]any{}

	used, _ := info.Num("used_memory")
	maxmem, _ := info.Num("maxmemory")
	total, _ := info.Num("total_system_memory")
	memPct, memKnown := 0.0, false
	switch {
	case maxmem > 0:
		memPct, memKnown = used/maxmem*100, true
	case total > 0:
		memPct, memKnown = used/total*100, true
	}
	if memKnown {
		m["mem_pct"] = memPct
	}

	hits, _ := info.Num("keyspace_hits")
	misses, _ := info.Num("keyspace_misses")
	evicted, _ := info.Num("evicted_keys")
	cur := Sample{At: now, Hits: hits, Misses: misses, Evicted: evicted}

	hitRate, hitKnown := 0.0, false
	if prev != nil {
		dh, dm := hits-prev.Hits, misses-prev.Misses
		if dh >= 0 && dm >= 0 && dh+dm > 0 {
			hitRate, hitKnown = dh/(dh+dm)*100, true
		}
		if de := evicted - prev.Evicted; de >= 0 {
			if mins := now.Sub(prev.At).Minutes(); mins > 0 {
				m["evictions"] = de / mins
			}
		}
	}
	if !hitKnown && hits+misses > 0 {
		hitRate, hitKnown = hits/(hits+misses)*100, true
	}
	if hitKnown {
		m["hit_rate"] = hitRate
	}
	if c, ok := info.Num("connected_clients"); ok {
		m["clients"] = c
	}

	policy := info.Str("maxmemory_policy")
	var conds []model.Condition
	if memKnown && memPct >= cacheFullMemPct && (policy == "noeviction" || (hitKnown && hitRate < 50)) {
		d := fmt.Sprintf("memory %.1f%% of %s", memPct, humanBytes(maxOr(maxmem, total)))
		if policy != "" {
			d += ", policy " + policy
		}
		conds = append(conds, model.Condition{Kind: model.CondCacheFull, Ref: "policy/" + policy, Detail: d})
	}

	if h := info.Str("maxmemory_human"); h != "" {
		detail["maxmemory"] = h
	} else if maxmem > 0 {
		detail["maxmemory"] = humanBytes(maxmem)
	}
	if policy != "" {
		detail["maxmemory_policy"] = policy
	}
	if h := info.Str("used_memory_human"); h != "" {
		detail["used_memory_human"] = h
	}
	if v := info.Str("redis_version"); v != "" {
		detail["redis_version"] = v
	}
	keys, expires := info.Keys()
	detail["keys"] = keys
	detail["keys_without_ttl"] = keys - expires
	return m, conds, detail, cur
}

func maxOr(a, b float64) float64 {
	if a > 0 {
		return a
	}
	return b
}

// humanBytes renders a byte count the way redis-cli does ("2.00G").
func humanBytes(b float64) string {
	units := []string{"B", "K", "M", "G", "T"}
	i := 0
	for b >= 1024 && i < len(units)-1 {
		b /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f%s", b, units[i])
	}
	return fmt.Sprintf("%.2f%s", b, units[i])
}

// infoAccess documents redis.info.
var infoAccess = probe.Access{
	Kind:   "redis.info",
	Source: "the Redis INFO command",
	Delivers: "mem_pct, hit_rate (per tick), evictions (per minute), clients; " +
		"CacheFull when memory is at the limit and the policy is noeviction or the hit rate collapsed; " +
		"detail: maxmemory, maxmemory_policy, used_memory_human, keys, keys_without_ttl, redis_version",
	SpecFields:  []string{"addr", "url", "password_env", "db", "tls"},
	Needs:       "network access to the Redis port; a password in the environment variable named by password_env when AUTH is on",
	Implemented: true,
}

func init() {
	probe.Register(infoAccess, func() probe.Probe { return &InfoProbe{} })
}

// InfoProbe is redis.info. Spec: addr (host:port) or url (redis://…),
// password_env, db, tls.
type InfoProbe struct {
	h probe.Health
}

// Kind implements probe.Probe.
func (p *InfoProbe) Kind() string { return infoAccess.Kind }

// Validate implements probe.Probe.
func (p *InfoProbe) Validate(spec map[string]any) error { return validateOptions(spec, "url") }

// Health implements probe.Probe.
func (p *InfoProbe) Health() probe.ProbeHealth { return p.h.Get() }

// Start implements probe.Probe.
func (p *InfoProbe) Start(ctx context.Context, spec map[string]any, out chan<- probe.Observation) error {
	opt, err := options(spec, "url")
	if err != nil {
		p.h.Set(probe.HealthFailed, err.Error())
		return err
	}
	client := redis.NewClient(opt)
	tgt := target(spec)
	every := tick(spec)
	go func() {
		defer client.Close()
		var prev *Sample
		seen := firstSeen{}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			o := probe.Observation{Target: tgt, Probe: p.Kind(), At: time.Now()}
			rctx, cancel := context.WithTimeout(ctx, roundTimeout)
			text, err := client.Info(rctx).Result()
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				o.Err = "INFO: " + err.Error()
				p.h.Set(probe.HealthDegraded, o.Err)
			} else {
				info := ParseInfo(text)
				m, conds, detail, cur := Observe(info, prev, o.At)
				prev = &cur
				live := map[string]bool{}
				for i := range conds {
					live[conds[i].Kind] = true
					conds[i].Since = seen.mark(conds[i].Kind, o.At)
				}
				seen.keep(live)
				o.Metrics, o.Conditions, o.Detail = m, conds, detail
				p.h.Set(probe.HealthOK, "")
			}
			if !probe.Send(ctx, out, o) {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}
