// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

// A refusal that names the missing permission says how to get a key with it;
// one that does not adds nothing.
func TestResponseErrorSuggestsTheMissingPermission(t *testing.T) {
	refused := func(permission string) httpapi.Error {
		return httpapi.Error{Code: "forbidden", Reason: "authorization", Error: "this API key does not have the " + permission + " permission",
			Details: []httpapi.ErrorDetail{{Type: "missing_permission", Metadata: httpapi.ErrorDetailMetadata{"permission": permission}}}}
	}
	for permission, want := range map[string]string{
		"write":          "gv login --permission write",
		"manage_members": "gv login --org ORGANIZATION --permission manage_members",
	} {
		if got := responseError(refused(permission), httpapi.NewOptString("r1")).Error(); !strings.Contains(got, want) || !strings.Contains(got, "(forbidden; request r1)") {
			t.Errorf("%s: %q, want it to contain %q", permission, got, want)
		}
	}
	plain := httpapi.Error{Code: "forbidden", Reason: "authorization", Error: "the action is not permitted"}
	if got := responseError(plain, httpapi.OptString{}).Error(); got != "the action is not permitted (forbidden)" {
		t.Errorf("plain refusal: %q", got)
	}
}
