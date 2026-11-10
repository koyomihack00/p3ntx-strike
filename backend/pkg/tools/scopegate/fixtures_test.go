package scopegate

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// fakeApprover records each request and answers with a fixed verdict. It returns
// ctx.Err() once the context is done.
type fakeApprover struct {
	mu       sync.Mutex
	requests []ApprovalRequest
	approve  bool
	err      error
	block    bool
}

func (f *fakeApprover) Approve(ctx context.Context, req ApprovalRequest) (bool, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.block {
		<-ctx.Done()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return f.approve, f.err
}

func (f *fakeApprover) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func labPolicy() Policy {
	return Policy{
		Scope: Scope{
			CIDRs: []string{"10.10.10.0/24", "192.168.56.101"},
			Hosts: []string{"target.lab", "*.corp.lab"},
		},
		AgentTools: map[string][]string{
			"pentester": {"terminal", "browser", "file", "hack_result", "web_search", "advice"},
			"searcher":  {"web_search", "search_result"},
		},
		AutoApproveBinaries: append([]string(nil), SafeLocalBinaries...),
		ExternalSearch:      Allow,
	}
}

func newGate(t *testing.T, p Policy) *Gate {
	t.Helper()
	g, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func terminal(cmd string) Call {
	args, _ := json.Marshal(map[string]any{"input": cmd, "cwd": "/work", "detach": false, "timeout": 0, "message": "m"})
	return Call{FlowID: 7, Agent: "pentester", Tool: "terminal", Args: args}
}

func browser(url string) Call {
	args, _ := json.Marshal(map[string]any{"url": url, "action": "markdown", "message": "m"})
	return Call{FlowID: 7, Agent: "pentester", Tool: "browser", Args: args}
}
