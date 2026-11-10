package scopegate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"pentagi/pkg/tools"
)

var ErrDenied = errors.New("tool call denied by scope gate")

var _ tools.CallAuthorizer = (*Gate)(nil)

type Call struct {
	FlowID int64
	Agent  string
	Tool   string
	Args   json.RawMessage
}

type Decision struct {
	Verdict Verdict
	Reasons []string
	Targets []Target
	// Fingerprint binds an approval to this exact flow, agent, tool and argument
	// set; argument key order does not change it.
	Fingerprint string
}

type ApprovalRequest struct {
	Call     Call
	Decision Decision
}

// Approver asks the owner. It must honour ctx; an error or a cancelled ctx denies.
type Approver interface {
	Approve(ctx context.Context, req ApprovalRequest) (bool, error)
}

type toolClass int

const (
	classUnknown toolClass = iota
	classNoTarget
	classTerminal
	classBrowser
	classExternalSearch
)

// toolClasses must name every tool the registry defines: an unlisted tool is denied.
var toolClasses = map[string]toolClass{
	tools.FinalyToolName:             classNoTarget,
	tools.AskUserToolName:            classNoTarget,
	tools.MaintenanceToolName:        classNoTarget,
	tools.MaintenanceResultToolName:  classNoTarget,
	tools.CoderToolName:              classNoTarget,
	tools.CodeResultToolName:         classNoTarget,
	tools.PentesterToolName:          classNoTarget,
	tools.HackResultToolName:         classNoTarget,
	tools.AdviceToolName:             classNoTarget,
	tools.MemoristToolName:           classNoTarget,
	tools.MemoristResultToolName:     classNoTarget,
	tools.SearchToolName:             classNoTarget,
	tools.SearchResultToolName:       classNoTarget,
	tools.EnricherResultToolName:     classNoTarget,
	tools.SearchInMemoryToolName:     classNoTarget,
	tools.SearchGuideToolName:        classNoTarget,
	tools.StoreGuideToolName:         classNoTarget,
	tools.SearchAnswerToolName:       classNoTarget,
	tools.StoreAnswerToolName:        classNoTarget,
	tools.SearchCodeToolName:         classNoTarget,
	tools.StoreCodeToolName:          classNoTarget,
	tools.GraphitiSearchToolName:     classNoTarget,
	tools.ReportResultToolName:       classNoTarget,
	tools.SubtaskListToolName:        classNoTarget,
	tools.SubtaskPatchToolName:       classNoTarget,
	tools.FileToolName:               classNoTarget,
	tools.GetFlowStatusToolName:      classNoTarget,
	tools.StopFlowToolName:           classNoTarget,
	tools.SubmitFlowInputToolName:    classNoTarget,
	tools.PatchFlowSubtasksToolName:  classNoTarget,
	tools.WaitFlowCompletionToolName: classNoTarget,
	tools.TerminalToolName:           classTerminal,
	tools.BrowserToolName:            classBrowser,
	tools.GoogleToolName:             classExternalSearch,
	tools.DuckDuckGoToolName:         classExternalSearch,
	tools.TavilyToolName:             classExternalSearch,
	tools.FirecrawlToolName:          classExternalSearch,
	tools.TraversaalToolName:         classExternalSearch,
	tools.PerplexityToolName:         classExternalSearch,
	tools.SearxngToolName:            classExternalSearch,
	tools.SploitusToolName:           classExternalSearch,
	tools.WebSearchToolName:          classExternalSearch,
}

type Gate struct {
	policy *compiledPolicy
}

func New(p Policy) (*Gate, error) {
	cp, err := compile(p)
	if err != nil {
		return nil, err
	}
	return &Gate{policy: cp}, nil
}

func (g *Gate) Evaluate(call Call) Decision {
	d := Decision{Verdict: Allow, Fingerprint: fingerprint(call)}
	lower := func(v Verdict, reason string) {
		d.Verdict = min(d.Verdict, v)
		d.Reasons = append(d.Reasons, reason)
	}

	if !g.policy.toolAllowed(call.Agent, call.Tool) {
		lower(Deny, fmt.Sprintf("tool %q is not allowed for agent %q", call.Tool, call.Agent))
		return d
	}

	switch toolClasses[call.Tool] {
	case classNoTarget:
	case classExternalSearch:
		if g.policy.externalSearch != Allow {
			lower(g.policy.externalSearch, fmt.Sprintf("tool %q sends the query to an external search service", call.Tool))
		}
	case classBrowser:
		var args tools.Browser
		if err := json.Unmarshal(call.Args, &args); err != nil {
			lower(Deny, fmt.Sprintf("unparseable %q arguments: %v", call.Tool, err))
			return d
		}
		target, err := urlTarget(args.Url)
		if err != nil {
			lower(Deny, err.Error())
			return d
		}
		g.judgeTargets(&d, []Target{target}, false, lower)
	case classTerminal:
		var args tools.TerminalAction
		if err := json.Unmarshal(call.Args, &args); err != nil {
			lower(Deny, fmt.Sprintf("unparseable %q arguments: %v", call.Tool, err))
			return d
		}
		g.judgeCommand(&d, args.Input, lower)
	default:
		lower(Deny, fmt.Sprintf("tool %q has no scope classification", call.Tool))
	}

	return d
}

// judgeCommand allows a command only when the lexer understood all of it and
// every segment runs an auto-approved local utility; those cannot reach the
// network, so their arguments are not read as targets. Anything else needs
// approval, and a definite out-of-scope target in it is denied outright.
func (g *Gate) judgeCommand(d *Decision, input string, lower func(Verdict, string)) {
	facts := analyzeCommand(input)

	var unapproved []string
	for _, name := range facts.commands {
		if _, ok := g.policy.autoApprove[name]; name == "" || !ok {
			unapproved = append(unapproved, name)
		}
	}
	if len(facts.opaque) == 0 && len(unapproved) == 0 {
		return
	}

	for _, reason := range facts.opaque {
		lower(RequireApproval, "command not fully understood: "+reason)
	}
	for _, name := range unapproved {
		if name != "" {
			lower(RequireApproval, fmt.Sprintf("command runs %q, which is not auto-approved", name))
		}
	}
	for _, word := range facts.unparsed {
		lower(RequireApproval, fmt.Sprintf("cannot parse possible target %q", word))
	}
	g.judgeTargets(d, facts.targets, true, lower)
}

func (g *Gate) judgeTargets(d *Decision, targets []Target, skipLocal bool, lower func(Verdict, string)) {
	for _, t := range targets {
		if skipLocal && t.localOnly() {
			continue
		}
		d.Targets = append(d.Targets, t)
		if g.policy.targetInScope(t) {
			continue
		}
		if t.Definite {
			lower(Deny, fmt.Sprintf("target %q is out of scope", t.Value))
		} else {
			lower(RequireApproval, fmt.Sprintf("possible target %q is not in scope", t.Value))
		}
	}
}

// Authorize returns a nil error only when the call may run now. An approval can lift
// RequireApproval; nothing lifts Deny.
func (g *Gate) Authorize(ctx context.Context, call Call, approver Approver) (Decision, error) {
	d := g.Evaluate(call)
	switch d.Verdict {
	case Allow:
		return d, nil
	case RequireApproval:
		if approver == nil {
			return d, fmt.Errorf("%w: approval required but no approver: %s", ErrDenied, strings.Join(d.Reasons, "; "))
		}
		ok, err := approver.Approve(ctx, ApprovalRequest{Call: call, Decision: d})
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			return d, fmt.Errorf("%w: approval not obtained: %w", ErrDenied, err)
		}
		if !ok {
			return d, fmt.Errorf("%w: owner rejected: %s", ErrDenied, strings.Join(d.Reasons, "; "))
		}
		return d, nil
	default:
		return d, fmt.Errorf("%w: %s", ErrDenied, strings.Join(d.Reasons, "; "))
	}
}

// AuthorizeToolCall is the executor's view of Authorize. With no approval
// mechanism yet, a call that needs approval is refused like a denied one.
func (g *Gate) AuthorizeToolCall(ctx context.Context, call tools.ToolCall) error {
	_, err := g.Authorize(ctx, Call{
		FlowID: call.FlowID,
		Agent:  string(call.Agent),
		Tool:   call.Tool,
		Args:   call.Args,
	}, nil)
	return err
}

func fingerprint(call Call) string {
	args := string(call.Args)
	var parsed any
	if err := json.Unmarshal(call.Args, &parsed); err == nil {
		if canonical, err := json.Marshal(parsed); err == nil {
			args = string(canonical)
		}
	}
	sum := sha256.Sum256([]byte(strconv.FormatInt(call.FlowID, 10) + "\x00" + call.Agent + "\x00" + call.Tool + "\x00" + args))
	return hex.EncodeToString(sum[:])
}
