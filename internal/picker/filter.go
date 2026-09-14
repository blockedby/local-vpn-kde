package picker

import "strings"

// FilterOptions describes user-facing node filters shared by test, pick, list and apply best.
type FilterOptions struct {
	Include        []string
	Exclude        []string
	Transport      []string
	Security       []string
	MinMbps        float64
	DefaultExclude bool
}

var DefaultExcludeTerms = []string{"Россия", "🇷🇺", "Russia", " RU ", "剩余", "流量", "官网", "套餐", "到期", "expire", "traffic", "subscription"}

func (f FilterOptions) MatchResult(r NodeResult) (bool, []string) {
	var reasons []string
	text := strings.ToLower(r.Name + " " + r.Host)
	if len(f.Include) > 0 && !containsAny(text, f.Include) {
		reasons = append(reasons, "include")
	}
	if containsAny(text, f.Exclude) {
		reasons = append(reasons, "exclude")
	}
	if f.DefaultExclude && containsAny(text, DefaultExcludeTerms) {
		reasons = append(reasons, "default_exclude")
	}
	if len(f.Transport) > 0 && !containsExact(strings.ToLower(r.Network), f.Transport) {
		reasons = append(reasons, "transport")
	}
	if len(f.Security) > 0 && !containsExact(strings.ToLower(r.Security), f.Security) {
		reasons = append(reasons, "security")
	}
	if f.MinMbps > 0 && r.Mbps < f.MinMbps {
		reasons = append(reasons, "min_mbps")
	}
	return len(reasons) == 0, reasons
}

func (f FilterOptions) Apply(rs []NodeResult) []NodeResult {
	out := make([]NodeResult, len(rs))
	for i, r := range rs {
		ok, reasons := f.MatchResult(r)
		r.Excluded = !ok
		r.ExcludeReasons = reasons
		out[i] = r
	}
	return out
}

func BestFiltered(rs []NodeResult) *NodeResult {
	var b *NodeResult
	for i := range rs {
		if rs[i].OK && !rs[i].Excluded && (b == nil || rs[i].Mbps > b.Mbps) {
			b = &rs[i]
		}
	}
	return b
}

func containsAny(text string, needles []string) bool {
	for _, n := range needles {
		n = strings.ToLower(strings.TrimSpace(n))
		if n != "" && strings.Contains(text, n) {
			return true
		}
	}
	return false
}

func containsExact(v string, opts []string) bool {
	for _, o := range opts {
		if v == strings.ToLower(strings.TrimSpace(o)) {
			return true
		}
	}
	return false
}
