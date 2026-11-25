package main

import "testing"

func TestNormalizeKeywordOptions(t *testing.T) {
	cases := map[string]string{
		"k3d,cpus=2":                           "k3d:cpus=2",
		"valkey,docker-image":                  "valkey:docker-image",
		"k3d":                                  "k3d",
		"k3d:cpus=2,nodes=3":                   "k3d:cpus=2,nodes=3",
		"pmm-client:3.7.0-7,server=node0:8443": "pmm-client:3.7.0-7,server=node0:8443",
		"haproxy-pg:node1,node2":               "haproxy-pg:node1,node2",
		"os:el9":                               "os:el9",
		"pg,wal=logical,master=node0":          "pg:wal=logical,master=node0",
	}
	for in, want := range cases {
		if got := normalizeKeywordOptions(in); got != want {
			t.Errorf("normalizeKeywordOptions(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApplyGlobalK3dCPUs(t *testing.T) {
	args := map[string]string{}
	applyGlobalK3dCPUs(args, "2")
	if args["cpus"] != "2" {
		t.Errorf("--cpus 2 not applied: %v", args)
	}
	args = map[string]string{"cpus": "1.5"}
	applyGlobalK3dCPUs(args, "2")
	if args["cpus"] != "1.5" {
		t.Errorf("cpus= on the keyword must win over --cpus: %v", args)
	}
	args = map[string]string{}
	applyGlobalK3dCPUs(args, "")
	if _, set := args["cpus"]; set {
		t.Errorf("no --cpus must leave cpus unset: %v", args)
	}
}

func TestQuotedDeployValues(t *testing.T) {
	if p := quotedDeployValues([]string{"node0", "ps:8.4,password=a'b"}); len(p) != 1 {
		t.Errorf("expected one problem for a quote in a value, got %v", p)
	}
	if p := quotedDeployValues([]string{"node0", "ps:8.4,password=secret", "os:el9"}); len(p) != 0 {
		t.Errorf("expected no problems, got %v", p)
	}
}

func TestIsDeploymentVersionOptions(t *testing.T) {
	for _, arg := range []string{"nodes=2", "cpus=2", "master=node0"} {
		if IsDeploymentVersion(arg) {
			t.Errorf("IsDeploymentVersion(%q) = true, an option is not a version", arg)
		}
	}
	for _, arg := range []string{"8.4", "v1.30.4-k3s1", "node0", "default", "latest-not"} {
		want := arg != "latest-not"
		if got := IsDeploymentVersion(arg); got != want {
			t.Errorf("IsDeploymentVersion(%q) = %v, want %v", arg, got, want)
		}
	}
}
