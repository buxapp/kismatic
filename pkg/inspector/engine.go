package inspector

import (
	"fmt"
	"sync"

	"github.com/apprenda/kismatic-platform/pkg/inspector/check"
)

// RuleCheckMapper implements a mapping between a
// rule and a check.
type RuleCheckMapper interface {
	GetCheckForRule(Rule) (check.Check, error)
}

// The DefaultCheckMapper contains the mappings for all
// supported rules and checks.
type DefaultCheckMapper struct {
	PackageManager check.PackageManager
}

// GetCheckForRule returns the check for the given rule. If the rule
// is unknown to the mapper, it returns an error.
func (m DefaultCheckMapper) GetCheckForRule(rule Rule) (check.Check, error) {
	var c check.Check
	switch r := rule.(type) {
	default:
		return nil, fmt.Errorf("Rule of type %T is not supported", r)
	case PackageInstalled:
		pkgQuery := check.PackageQuery{Name: r.PackageName, Version: r.PackageVersion}
		c = &check.PackageInstalledCheck{pkgQuery, m.PackageManager}
	case PackageAvailable:
		pkgQuery := check.PackageQuery{Name: r.PackageName, Version: r.PackageVersion}
		c = &check.PackageAvailableCheck{pkgQuery, m.PackageManager}
	case ExecutableInPath:
		c = &check.BinaryDependencyCheck{r.Executable}
	case FileContentMatches:
		c = check.FileContentCheck{File: r.File, SearchString: r.ContentRegex}
	case TCPPortAvailable:
		c = &check.TCPPortServerCheck{PortNumber: r.Port}
	case TCPPortAccessible:
		c = &check.TCPPortClientCheck{PortNumber: r.Port}
	}
	return c, nil
}

// The Engine executes rules and reports the results
type Engine struct {
	RuleCheckMapper RuleCheckMapper
	mu              sync.Mutex
	closableChecks  []check.ClosableCheck
}

// ExecuteRules runs the rules that should be executed according to the facts,
// and returns a collection of results. The number of results is not guaranteed
// to equal the number of rules.
func (e *Engine) ExecuteRules(rules []Rule, facts []string) ([]RuleResult, error) {
	results := []RuleResult{}
	for _, rule := range rules {
		if !shouldExecuteRule(rule, facts) {
			continue
		}

		// Map the rule to a check
		c, err := e.RuleCheckMapper.GetCheckForRule(rule)
		if err != nil {
			return nil, err
		}

		// We update the closables as we go to avoid leaking closables
		// in the event where we have to return an error from within the loop.
		if closeable, ok := c.(check.ClosableCheck); ok {
			e.mu.Lock()
			e.closableChecks = append(e.closableChecks, closeable)
			e.mu.Unlock()
		}

		// Run the check and report result
		ok, err := c.Check()
		res := RuleResult{
			Name:        rule.Name(),
			Success:     ok,
			Error:       err,
			Remediation: "",
		}
		results = append(results, res)
	}
	return results, nil
}

// CloseChecks that need to be closed
func (e *Engine) CloseChecks() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, c := range e.closableChecks {
		if err := c.Close(); err != nil {
			// TODO: Figure out what to do with the error here
		}
	}
	return nil
}

func shouldExecuteRule(rule Rule, facts []string) bool {
	if len(rule.GetRuleMeta().When) == 0 {
		// No conditions on the rule => always run
		return true
	}
	// Run if and only if the all the conditions on the rule are
	// satisfied by the facts
	for _, whenCondition := range rule.GetRuleMeta().When {
		found := false
		for _, l := range facts {
			if whenCondition == l {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
