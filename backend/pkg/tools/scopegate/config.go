package scopegate

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"pentagi/pkg/config"
	"pentagi/pkg/database"
)

// gatedAgents are the agents that own a tool executor. Each may call any
// classified tool: which tools an agent is offered is fixed where its executor is
// built, so the gate's job here is the scope, not a second copy of those lists.
var gatedAgents = []database.MsgchainType{
	database.MsgchainTypeAssistant,
	database.MsgchainTypePrimaryAgent,
	database.MsgchainTypeInstaller,
	database.MsgchainTypeCoder,
	database.MsgchainTypePentester,
	database.MsgchainTypeSearcher,
	database.MsgchainTypeGenerator,
	database.MsgchainTypeRefiner,
	database.MsgchainTypeMemorist,
	database.MsgchainTypeEnricher,
	database.MsgchainTypeReporter,
}

// PolicyFromConfig fails on any entry it cannot parse rather than dropping it, so
// a typo in the scope stops the server instead of narrowing or widening the scope.
func PolicyFromConfig(cfg *config.Config) (Policy, error) {
	search, err := parseExternalSearch(cfg.ScopeGateExternalSearch)
	if err != nil {
		return Policy{}, err
	}

	classified := slices.Sorted(maps.Keys(toolClasses))
	agentTools := make(map[string][]string, len(gatedAgents))
	for _, agent := range gatedAgents {
		agentTools[string(agent)] = classified
	}

	return Policy{
		Scope: Scope{
			CIDRs: nonBlank(cfg.ScopeGateCIDRs),
			Hosts: nonBlank(cfg.ScopeGateHosts),
		},
		AgentTools:          agentTools,
		AutoApproveBinaries: slices.Clone(SafeLocalBinaries),
		ExternalSearch:      search,
	}, nil
}

func FromConfig(cfg *config.Config) (*Gate, error) {
	p, err := PolicyFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	return New(p)
}

// parseExternalSearch has no "require_approval": without an approver it would
// only be a slower spelling of deny.
func parseExternalSearch(raw string) (Verdict, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "deny":
		return Deny, nil
	case "allow":
		return Allow, nil
	default:
		return Deny, fmt.Errorf("invalid SCOPE_GATE_EXTERNAL_SEARCH %q: want allow or deny", raw)
	}
}

// nonBlank drops the empty items a trailing or doubled comma leaves; an item that
// is not blank is kept verbatim so New judges it.
func nonBlank(items []string) []string {
	var out []string
	for _, item := range items {
		if strings.TrimSpace(item) != "" {
			out = append(out, item)
		}
	}
	return out
}
