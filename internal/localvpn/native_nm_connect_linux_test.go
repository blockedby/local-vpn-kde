//go:build linux

package localvpn

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeConnectImportsOnlyProvenProfiles(t *testing.T) {
	for _, scenario := range []string{"missing", "stale", "refresh", "existing", "foreign", "tampered", "other-vpn", "denied", "route-conflict", "activation-failed", "metric-upgrade", "route-upgrade"} {
		t.Run(scenario, func(t *testing.T) {
			n := NetworkManager{Base: t.TempDir(), Native: true}
			if err := os.MkdirAll(filepath.Dir(n.profilePath()), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(n.profilePath(), []byte("client\ndev tun\nproto udp\nremote 127.0.0.1 21194\n"), 0600); err != nil {
				t.Fatal(err)
			}
			fp, err := n.profileFingerprint()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			metric := "0"
			routes := nativeSplitRoutes()
			neverDefault := "yes"
			if scenario == "metric-upgrade" || scenario == "route-upgrade" {
				routes = ""
				neverDefault = "no"
			}
			if scenario == "metric-upgrade" {
				metric = "50"
			}
			old := "11111111-1111-4111-8111-111111111111"
			fresh := "22222222-2222-4222-8222-222222222222"
			names := map[string]string{}
			active := ""
			imports, ups, deletes, modifies, downs := 0, 0, 0, 0, 0
			switch scenario {
			case "stale":
				if err = n.writeCapability(nmCapability{UUID: old, Fingerprint: fp}); err != nil {
					t.Fatal(err)
				}
			case "refresh", "existing", "tampered", "metric-upgrade", "route-upgrade":
				saved := fp
				if scenario == "refresh" {
					saved = strings.Repeat("0", 64)
				}
				if err = n.writeCapability(nmCapability{UUID: old, Fingerprint: saved}); err != nil {
					t.Fatal(err)
				}
				names[old] = "vpnkit-native"
			case "foreign":
				names[old] = "vpnkit-native"
			}
			n.command = func(commandCtx context.Context, args ...string) (string, error) {
				joined := strings.Join(args, " ")
				switch {
				case joined == "-t -f NAME,UUID,TYPE connection show":
					var out strings.Builder
					for id, name := range names {
						out.WriteString(name + ":" + id + ":vpn\n")
					}
					return out.String(), nil
				case joined == "-t -f NAME,UUID,TYPE,DEVICE connection show --active":
					prefix := ""
					if scenario == "other-vpn" || scenario == "route-conflict" {
						prefix = "work:" + old + ":vpn:tun1\n"
					}
					if active != "" {
						return prefix + names[active] + ":" + active + ":vpn:tun0\n", nil
					}
					return prefix, nil
				case strings.HasPrefix(joined, "-t -f connection.id,"):
					id := args[len(args)-1]
					remote := "127.0.0.1"
					if scenario == "tampered" {
						remote = "192.0.2.1"
					}
					return names[id] + ":" + id + ":vpn:org.freedesktop.NetworkManager.openvpn:remote = " + remote + ":21194\n", nil
				case strings.HasPrefix(joined, "connection import "):
					imports++
					if scenario == "denied" {
						return "Not authorized to control networking.", errors.New("nmcli failed")
					}
					names[fresh] = "imported"
					return fresh, nil
				case strings.HasPrefix(joined, "-g ipv4.route-metric "):
					return metric, nil
				case strings.HasPrefix(joined, "-g ipv4.routes "):
					return routes, nil
				case strings.HasPrefix(joined, "-g ipv4.never-default "):
					return neverDefault, nil
				case strings.HasPrefix(joined, "connection down uuid "):
					downs++
					if commandCtx.Err() != nil {
						t.Fatal("rollback inherited canceled context")
					}
					if _, ok := commandCtx.Deadline(); !ok {
						t.Fatal("rollback context unbounded")
					}
					active = ""
					return "", nil
				case strings.HasPrefix(joined, "connection modify uuid "):
					modifies++
					for i := 4; i+1 < len(args); i += 2 {
						switch args[i] {
						case "connection.id":
							names[args[3]] = args[i+1]
						case "ipv4.route-metric":
							metric = args[i+1]
						case "ipv4.routes":
							routes = args[i+1]
						case "ipv4.never-default":
							neverDefault = args[i+1]
						}
					}
					return "", nil
				case strings.HasPrefix(joined, "connection delete uuid "):
					deletes++
					delete(names, args[3])
					return "", nil
				case strings.HasPrefix(joined, "--wait 0 connection up uuid "):
					ups++
					active = args[len(args)-1]
					if scenario == "activation-failed" {
						cancel()
						return "", errors.New("activation reply lost")
					}
					return "", nil
				case strings.HasPrefix(joined, "-t -f IP4.ADDRESS "):
					return "10.80.0.2/24\n", nil
				default:
					t.Fatalf("unexpected NM command %q", joined)
					return "", errors.New("unexpected")
				}
			}
			bin := t.TempDir()
			ipScript := "#!/bin/sh\nif [ \"$1\" = -json ]; then echo '[{\"dst\":\"0.0.0.0/1\"},{\"dst\":\"128.0.0.0/1\"}]'; elif [ \"$1\" = -o ]; then printf '7: tun0 inet 10.80.0.2/24 scope global tun0\\n'; else printf '%s dev tun0 src 10.80.0.2\\n' \"$4\"; fi\n"
			if scenario == "route-conflict" {
				ipScript = strings.Replace(ipScript, "else printf", "elif [ \"$4\" = 208.67.222.222 ]; then printf '%s dev tun1\\n' \"$4\"; else printf", 1)
			}
			if err = os.WriteFile(filepath.Join(bin, "ip"), []byte(ipScript), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			var out bytes.Buffer
			if scenario == "missing" || scenario == "stale" || scenario == "refresh" {
				events := []string{}
				service := &fakeNativeDesktopService{events: &events}
				installNativeDesktopFakes(t, service, &n)
				err = RunNativeDesktop(ctx, n.Base, "start", true, &out)
				if len(events) != 2 || events[0] != "service-status" || events[1] != "service-start" {
					t.Fatalf("cold lifecycle events=%v", events)
				}
			} else {
				err = n.Run(ctx, "connect", true, &out)
			}
			success := scenario == "missing" || scenario == "stale" || scenario == "refresh" || scenario == "existing" || scenario == "other-vpn" || scenario == "metric-upgrade" || scenario == "route-upgrade"
			if (err == nil) != success {
				t.Fatalf("connect error=%v", err)
			}
			if scenario == "route-conflict" || scenario == "activation-failed" {
				if err == nil || ups != 1 || downs != 1 {
					t.Fatalf("failed activation not rolled back: err=%v up=%d down=%d", err, ups, downs)
				}
				return
			}
			if !success {
				if ups != 0 || deletes != 0 || modifies != 0 {
					t.Fatal("refused connection mutated NM")
				}
				if scenario == "denied" {
					if !errors.Is(err, ErrNMAuthorization) || strings.Contains(err.Error(), "unique new UUID") {
						t.Fatalf("authorization masked: %v", err)
					}
				} else if imports != 0 {
					t.Fatal("unsafe import attempted")
				}
				return
			}
			want := 1
			if scenario == "existing" || scenario == "metric-upgrade" || scenario == "route-upgrade" {
				want = 0
			}
			if !nativeSplitRoutesMatch(routes) || neverDefault != "yes" || metric != "0" {
				t.Fatal("owned native split route policy not provisioned")
			}
			if imports != want || ups != 1 {
				t.Fatalf("imports=%d ups=%d", imports, ups)
			}
			if (scenario == "metric-upgrade" || scenario == "route-upgrade") && modifies != 1 {
				t.Fatal("legacy owned route metric not upgraded exactly once")
			}
			if scenario == "refresh" && deletes != 1 {
				t.Fatal("old owned profile not replaced")
			}
			out.Reset()
			if err = n.Run(ctx, "connect", true, &out); err != nil {
				t.Fatal(err)
			}
			if imports != want || ups != 1 || !strings.Contains(out.String(), "already-connected") {
				t.Fatal("repeat connect mutated existing connection")
			}
		})
	}
}

func TestNativeSplitRouteSettings(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{"0.0.0.0/1 10.90.0.1 0, 128.0.0.0/1 10.90.0.1 0", true},
		{"128.0.0.0/1   10.90.0.1 0,0.0.0.0/1 10.90.0.1 0", true},
		{"0.0.0.0/0 10.90.0.1 0", false},
		{"0.0.0.0/1 10.90.0.1 50, 128.0.0.0/1 10.90.0.1 50", false},
		{"0.0.0.0/1 10.89.0.1 0, 128.0.0.0/1 10.89.0.1 0", false},
		{"0.0.0.0/1 10.90.0.1 0, 0.0.0.0/1 10.90.0.1 0", false},
	} {
		if got := nativeSplitRoutesMatch(tc.raw); got != tc.valid {
			t.Fatalf("route setting %q accepted=%v", tc.raw, got)
		}
	}
}
