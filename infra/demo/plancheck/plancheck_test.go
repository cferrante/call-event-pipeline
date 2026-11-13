package main

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// guardScript stands in for the expiry guard the instances' user data must contain.
const guardScript = "#cloud-config\nbootcmd:\n  - systemd-run --unit=demo-expiry --on-active=10800s /bin/systemctl poweroff\n"

// goodPlan is a minimal plan in `tofu show -json` form that follows every rule: a security
// group without inline ingress, one ingress rule from another security group, and an instance
// that terminates on shutdown, schedules its own expiry and requires IMDSv2.
func goodPlan() map[string]any {
	return map[string]any{
		"resource_changes": []any{
			change("aws_security_group.sut", "aws_security_group", map[string]any{
				"name": "sut", "ingress": []any{},
				"egress": []any{map[string]any{"cidr_blocks": []any{"0.0.0.0/0"}}},
			}),
			change("aws_vpc_security_group_ingress_rule.ingest_from_loadgen", "aws_vpc_security_group_ingress_rule",
				map[string]any{"from_port": 8081, "to_port": 8081, "ip_protocol": "tcp"}),
			change("aws_instance.sut", "aws_instance", map[string]any{
				"instance_initiated_shutdown_behavior": "terminate",
				"user_data_base64":                     base64.StdEncoding.EncodeToString([]byte(guardScript)),
				"metadata_options":                     []any{map[string]any{"http_tokens": "required"}},
			}),
		},
	}
}

func change(address, typ string, after map[string]any) map[string]any {
	return map[string]any{
		"address": address, "type": typ,
		"change": map[string]any{"actions": []any{"create"}, "after": after},
	}
}

// after returns the planned values of the resource at address in plan p, for editing.
func after(t *testing.T, p map[string]any, address string) map[string]any {
	t.Helper()
	for _, rc := range p["resource_changes"].([]any) {
		rc := rc.(map[string]any)
		if rc["address"] == address {
			return rc["change"].(map[string]any)["after"].(map[string]any)
		}
	}
	t.Fatalf("no resource %s in plan", address)
	return nil
}

func checkPlan(t *testing.T, p map[string]any) []string {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	problems, err := Check(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return problems
}

func TestGoodPlanPasses(t *testing.T) {
	if problems := checkPlan(t, goodPlan()); len(problems) != 0 {
		t.Fatalf("want no problems, got %q", problems)
	}
}

func TestPlanViolations(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(t *testing.T, p map[string]any)
		want   string // a fragment of the reported problem
	}{
		{"inline ingress on a security group", func(t *testing.T, p map[string]any) {
			after(t, p, "aws_security_group.sut")["ingress"] = []any{map[string]any{"from_port": 22, "cidr_blocks": []any{"0.0.0.0/0"}}}
		}, "aws_security_group.sut allows inbound from [0.0.0.0/0]"},
		{"inline ingress from an IPv6 range", func(t *testing.T, p map[string]any) {
			after(t, p, "aws_security_group.sut")["ingress"] = []any{map[string]any{"from_port": 22, "ipv6_cidr_blocks": []any{"::/0"}}}
		}, "aws_security_group.sut allows inbound from [::/0]"},
		{"ingress rule from an IPv4 range", func(t *testing.T, p map[string]any) {
			after(t, p, "aws_vpc_security_group_ingress_rule.ingest_from_loadgen")["cidr_ipv4"] = "0.0.0.0/0"
		}, "ingest_from_loadgen allows inbound from 0.0.0.0/0"},
		{"ingress rule from an IPv6 range", func(t *testing.T, p map[string]any) {
			after(t, p, "aws_vpc_security_group_ingress_rule.ingest_from_loadgen")["cidr_ipv6"] = "::/0"
		}, "ingest_from_loadgen allows inbound from ::/0"},
		{"ingress rule from a prefix list", func(t *testing.T, p map[string]any) {
			after(t, p, "aws_vpc_security_group_ingress_rule.ingest_from_loadgen")["prefix_list_id"] = "pl-123"
		}, "ingest_from_loadgen allows inbound from pl-123"},
		{"legacy ingress rule from a range", func(t *testing.T, p map[string]any) {
			rcs := p["resource_changes"].([]any)
			p["resource_changes"] = append(rcs, change("aws_security_group_rule.ssh", "aws_security_group_rule",
				map[string]any{"type": "ingress", "cidr_blocks": []any{"203.0.113.0/24"}}))
		}, "aws_security_group_rule.ssh allows inbound from [203.0.113.0/24]"},
		{"instance stops instead of terminating", func(t *testing.T, p map[string]any) {
			after(t, p, "aws_instance.sut")["instance_initiated_shutdown_behavior"] = "stop"
		}, "aws_instance.sut does not terminate on shutdown"},
		{"instance with the shutdown behaviour unset (AWS's default is stop)", func(t *testing.T, p map[string]any) {
			delete(after(t, p, "aws_instance.sut"), "instance_initiated_shutdown_behavior")
		}, "aws_instance.sut does not terminate on shutdown"},
		{"instance without the expiry guard", func(t *testing.T, p map[string]any) {
			after(t, p, "aws_instance.sut")["user_data_base64"] = base64.StdEncoding.EncodeToString([]byte("#cloud-config\n"))
		}, "aws_instance.sut has no expiry guard"},
		{"instance with plain user data and no guard", func(t *testing.T, p map[string]any) {
			a := after(t, p, "aws_instance.sut")
			delete(a, "user_data_base64")
			a["user_data"] = "#!/bin/sh\necho hello\n"
		}, "aws_instance.sut has no expiry guard"},
		{"instance allows IMDSv1", func(t *testing.T, p map[string]any) {
			after(t, p, "aws_instance.sut")["metadata_options"] = []any{map[string]any{"http_tokens": "optional"}}
		}, "aws_instance.sut does not require IMDSv2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := goodPlan()
			tt.break_(t, p)
			problems := checkPlan(t, p)
			if len(problems) != 1 || !strings.Contains(problems[0], tt.want) {
				t.Fatalf("want one problem containing %q, got %q", tt.want, problems)
			}
		})
	}
}

// After the first apply, AWS reports the separate ingress rule inside the group's own ingress
// list. A rule from another security group, with no address range, is allowed.
func TestInlineIngressFromAnotherGroupPasses(t *testing.T) {
	p := goodPlan()
	after(t, p, "aws_security_group.sut")["ingress"] = []any{map[string]any{
		"from_port": 8081, "to_port": 8081, "protocol": "tcp", "security_groups": []any{"sg-0abc"},
		"cidr_blocks": []any{}, "ipv6_cidr_blocks": []any{}, "prefix_list_ids": []any{},
	}}
	if problems := checkPlan(t, p); len(problems) != 0 {
		t.Fatalf("want no problems, got %q", problems)
	}
}

// A resource being destroyed has no planned values and must not be reported.
func TestDeletedResourcesAreIgnored(t *testing.T) {
	p := goodPlan()
	rcs := p["resource_changes"].([]any)
	p["resource_changes"] = append(rcs, map[string]any{
		"address": "aws_instance.old", "type": "aws_instance",
		"change": map[string]any{"actions": []any{"delete"}, "after": nil},
	})
	if problems := checkPlan(t, p); len(problems) != 0 {
		t.Fatalf("want no problems, got %q", problems)
	}
}

func TestEmptyPlanIsAnError(t *testing.T) {
	if _, err := Check(strings.NewReader(`{"format_version":"1.2"}`)); err == nil {
		t.Fatal("want an error for a plan with no resource changes")
	}
}
