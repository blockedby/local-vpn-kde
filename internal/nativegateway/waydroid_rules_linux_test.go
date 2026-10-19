package nativegateway

import "testing"

func TestWaydroidRulesFollowNativeIsolation(t *testing.T) {
	if UnreachableRulePriority >= 5260 {
		t.Fatal("Waydroid rules precede the native fail-closed barrier")
	}
	_, err := parseRules([]byte(`[{"priority":0,"src":"all","table":"local"},{"priority":5260,"src":"192.168.240.0","srclen":24,"table":"main"},{"priority":5261,"src":"all","dst":"192.168.240.0","dstlen":24,"table":"main"}]`))
	if err != nil {
		t.Fatal(err)
	}
}
func TestSplitRuleSourcePrefixes(t *testing.T) {
	for _, tc := range []struct {
		rule    string
		blocked bool
	}{
		{`{"priority":100,"src":"192.168.240.0","srclen":24,"table":"main"}`, false},
		{`{"priority":100,"src":"172.30.89.0","srclen":24,"table":"main"}`, true},
		{`{"priority":100,"src":"172.30.89.2","table":"main"}`, true},
		{`{"priority":100,"src":"192.168.240.0","srclen":33,"table":"main"}`, true},
		{`{"priority":100,"src":"192.168.240.0","srclen":24,"not":true,"table":"main"}`, true},
		{`{"priority":100,"src":"all","dst":"192.168.240.0","dstlen":24,"table":"main"}`, true},
	} {
		_, err := parseRules([]byte("[" + tc.rule + "]"))
		if (err != nil) != tc.blocked {
			t.Errorf("%s: error=%v", tc.rule, err)
		}
	}
}
