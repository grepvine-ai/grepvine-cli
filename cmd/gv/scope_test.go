// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

// --expires never sends no expiry; without --expires, gv asks for 90 days.
func TestExpiresNever(t *testing.T) {
	request, err := (&scopeFlags{expires: "never"}).request("ci", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body, err := request.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "expires_at") {
		t.Fatalf("request = %s", body)
	}
	if got := describeKeyScope(&httpapi.DeviceToken{Permissions: []httpapi.Permission{"read"}}); got != "all organizations · all indexes · read · never expires" {
		t.Fatalf("describeKeyScope = %q", got)
	}
	now := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	request, err = (&scopeFlags{}).request("ci", now)
	if err != nil {
		t.Fatal(err)
	}
	if at, ok := request.ExpiresAt.Get(); !ok || !at.Equal(now.AddDate(0, 0, 90)) {
		t.Fatalf("default expiry = %v, want 90 days", request.ExpiresAt)
	}
	if got := fmt.Sprint(request.Permissions); got != "[search read]" {
		t.Fatalf("default permissions = %s, want search and read", got)
	}
}

// --agent needs --org, and a key acting as an agent says so.
func TestAgentScope(t *testing.T) {
	if _, err := (&scopeFlags{agent: "ci"}).request("ci", time.Now()); err == nil || !strings.Contains(err.Error(), "--agent needs --org") {
		t.Fatalf("--agent without --org: %v", err)
	}
	request, err := (&scopeFlags{organization: "acme", agent: "ci", expires: "never"}).request("ci", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if agent, _ := request.Agent.Get(); agent != "ci" {
		t.Fatalf("agent = %v", request.Agent)
	}
	key := &httpapi.DeviceToken{Organization: httpapi.NewOptName("acme"), Agent: httpapi.NewOptName("ci"), Permissions: []httpapi.Permission{"search"}}
	if got := describeKeyScope(key); got != "as agent ci of acme · all indexes · search · never expires" {
		t.Fatalf("describeKeyScope = %q", got)
	}
}
