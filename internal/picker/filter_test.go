package picker

import "testing"

func TestFilterOptionsApplyAndBestFiltered(t *testing.T) {
	rs := []NodeResult{
		{Index: 1, Name: "fast reality", Network: "tcp", Security: "reality", Mbps: 50, OK: true},
		{Index: 2, Name: "剩余流量", Network: "ws", Security: "tls", Mbps: 100, OK: true},
		{Index: 3, Name: "slow reality", Network: "tcp", Security: "reality", Mbps: 1, OK: true},
	}
	filtered := (FilterOptions{Transport: []string{"tcp"}, Security: []string{"reality"}, MinMbps: 10, DefaultExclude: true}).Apply(rs)
	if filtered[0].Excluded {
		t.Fatalf("first node should not be excluded: %#v", filtered[0].ExcludeReasons)
	}
	if !filtered[1].Excluded || !filtered[2].Excluded {
		t.Fatalf("expected second and third nodes excluded: %#v", filtered)
	}
	best := BestFiltered(filtered)
	if best == nil || best.Index != 1 {
		t.Fatalf("best filtered = %#v", best)
	}
}
