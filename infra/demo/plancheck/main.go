// Command plancheck reads an OpenTofu plan as JSON (`tofu show -json PLAN`) on stdin and
// fails if it would open the demo environment to inbound traffic or create an instance
// without its expiry guard. `make demo-up` runs it before every apply.
package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	problems, err := Check(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "plancheck:", err)
		os.Exit(2)
	}
	for _, p := range problems {
		fmt.Fprintln(os.Stderr, "plancheck: ✗", p)
	}
	if len(problems) > 0 {
		os.Exit(1)
	}
	fmt.Println("plancheck: ✓ no inbound access from any address; every instance terminates itself")
}

// expiryGuard is what an instance's user data must contain: a systemd timer, set at every
// boot, that powers the instance off (and so terminates it) when its time is up.
const expiryGuard = "systemd-run --unit=demo-expiry"

type plan struct {
	ResourceChanges []struct {
		Address string `json:"address"`
		Type    string `json:"type"`
		Change  struct {
			After map[string]any `json:"after"`
		} `json:"change"`
	} `json:"resource_changes"`
}

// Check returns every rule the plan breaks. Resources being destroyed have no planned
// values and are skipped.
func Check(r io.Reader) ([]string, error) {
	var p plan
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, fmt.Errorf("reading the plan: %w", err)
	}
	if len(p.ResourceChanges) == 0 {
		return nil, errors.New("the plan has no resource changes; is this `tofu show -json` of a plan?")
	}
	var problems []string
	for _, rc := range p.ResourceChanges {
		a := rc.Change.After
		if a == nil {
			continue
		}
		switch rc.Type {
		case "aws_security_group":
			// Inline entries: written in the config, or reported by AWS for separate rules
			// once they exist. Only another security group may be the source.
			entries, _ := a["ingress"].([]any)
			for _, e := range entries {
				m, _ := e.(map[string]any)
				problems = append(problems, fromRanges(rc.Address, m, "cidr_blocks", "ipv6_cidr_blocks", "prefix_list_ids")...)
			}
		case "aws_vpc_security_group_ingress_rule":
			problems = append(problems, fromRanges(rc.Address, a, "cidr_ipv4", "cidr_ipv6", "prefix_list_id")...)
		case "aws_security_group_rule":
			if a["type"] == "ingress" {
				problems = append(problems, fromRanges(rc.Address, a, "cidr_blocks", "ipv6_cidr_blocks", "prefix_list_ids")...)
			}
		case "aws_instance":
			if a["instance_initiated_shutdown_behavior"] != "terminate" {
				problems = append(problems, rc.Address+" does not terminate on shutdown")
			}
			if !strings.Contains(userData(a), expiryGuard) {
				problems = append(problems, rc.Address+" has no expiry guard in its user data")
			}
			if !requiresIMDSv2(a) {
				problems = append(problems, rc.Address+" does not require IMDSv2")
			}
		}
	}
	return problems, nil
}

// fromRanges reports each of keys that lets in traffic from an address range or prefix list.
func fromRanges(address string, rule map[string]any, keys ...string) []string {
	var problems []string
	for _, k := range keys {
		if !isEmpty(rule[k]) {
			problems = append(problems, fmt.Sprintf("%s allows inbound from %v", address, rule[k]))
		}
	}
	return problems
}

// userData returns an instance's user data as text, from either attribute.
func userData(a map[string]any) string {
	if s, ok := a["user_data_base64"].(string); ok && s != "" {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return ""
		}
		return string(b)
	}
	s, _ := a["user_data"].(string)
	return s
}

func requiresIMDSv2(a map[string]any) bool {
	opts, _ := a["metadata_options"].([]any)
	if len(opts) != 1 {
		return false
	}
	m, _ := opts[0].(map[string]any)
	return m["http_tokens"] == "required"
}

// isEmpty reports whether a planned value is absent, null, "" or an empty list.
func isEmpty(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	}
	return false
}
