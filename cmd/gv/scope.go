// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

// scopeFlags are what a new API key asks to reach: one organization or all of
// them, some of its indexes or all, a set of permissions, and an expiry. What
// a flag leaves out, the person approving the key in the browser sees and may
// change.
type scopeFlags struct {
	organization string
	indexes      []string
	permissions  []string
	expires      string
}

func (f *scopeFlags) register(command *cobra.Command) {
	command.Flags().StringVar(&f.organization, "org", "", "the one organization the key reaches (default: all of yours)")
	command.Flags().StringSliceVar(&f.indexes, "index", nil, "an index the key reaches, repeatable; needs --org (default: all indexes)")
	command.Flags().StringSliceVar(&f.permissions, "permission", nil, "search, read, write, create_index, manage_index, or with --org manage_members and manage_organization; repeatable (default: search and read)")
	command.Flags().StringVar(&f.expires, "expires", "", "when the key expires: a number of days such as 30d, a date such as 2027-01-31, or never (default: 90 days, at most a year)")
}

// defaultLifetime is how long a key gv asks for lives when --expires is not given.
const defaultLifetime = "90d"

// request is the device authorization request for the scope, at now.
func (f *scopeFlags) request(name string, now time.Time) (*httpapi.DeviceAuthorizationRequest, error) {
	request := &httpapi.DeviceAuthorizationRequest{APIKeyName: name}
	if f.organization != "" {
		request.Organization = httpapi.NewOptName(httpapi.Name(f.organization))
	}
	if len(f.indexes) > 0 {
		if f.organization == "" {
			return nil, fmt.Errorf("--index needs --org: index names belong to one organization")
		}
		names := make([]httpapi.IndexName, 0, len(f.indexes))
		for _, index := range f.indexes {
			names = append(names, httpapi.IndexName(index))
		}
		request.Indexes = names
	}
	// The service has no default permissions; gv's are search and read.
	permissions := f.permissions
	if len(permissions) == 0 {
		permissions = []string{"search", "read"}
	}
	for _, permission := range permissions {
		value := httpapi.Permission(permission)
		if err := value.Validate(); err != nil {
			return nil, fmt.Errorf("--permission %q is not one of search, read, write, create_index, manage_index, manage_members, manage_organization", permission)
		}
		request.Permissions = append(request.Permissions, value)
	}
	switch f.expires {
	case "never":
		// A key that works until it is revoked: no expiry sent.
	default:
		// The service has no default lifetime; gv's is 90 days.
		value := f.expires
		if value == "" {
			value = defaultLifetime
		}
		at, err := parseExpiry(value, now)
		if err != nil {
			return nil, err
		}
		request.ExpiresAt = httpapi.NewOptDateTime(at)
	}
	return request, nil
}

// parseExpiry reads a number of days ("30d") or a date ("2027-01-31", the end
// of that day in UTC) or a full RFC 3339 time.
func parseExpiry(value string, now time.Time) (time.Time, error) {
	if days, ok := strings.CutSuffix(value, "d"); ok {
		n, err := strconv.Atoi(days)
		if err == nil && n > 0 {
			return now.Add(time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if at, err := time.Parse(time.DateOnly, value); err == nil {
		return at.Add(24*time.Hour - time.Second), nil
	}
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at, nil
	}
	return time.Time{}, fmt.Errorf("--expires %q is not a number of days such as 30d, a date such as 2027-01-31, or never", value)
}

// describeKeyScope is one line that says what a key reaches.
// A field the service leaves out sets no limit.
func describeKeyScope(organization httpapi.OptName, indexes httpapi.IndexScope, permissions []httpapi.Permission, expires httpapi.OptDateTime) string {
	where := "all organizations"
	if name, ok := organization.Get(); ok {
		where = string(name)
	}
	which := "all indexes"
	if indexes != nil {
		names := make([]string, 0, len(indexes))
		for _, name := range indexes {
			names = append(names, string(name))
		}
		which = strings.Join(names, ", ")
	}
	granted := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		granted = append(granted, string(permission))
	}
	until := "never expires"
	if at, ok := expires.Get(); ok {
		until = "until " + at.UTC().Format(time.DateOnly)
	}
	return fmt.Sprintf("%s · %s · %s · %s", where, which, strings.Join(granted, ", "), until)
}
