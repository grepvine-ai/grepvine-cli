#!/bin/sh
# Fails when user-facing sources use terms that belong to the service, not to
# the public API. A reviewed hit is allowed by a `path:substring` line in
# scripts/check-public.allow.
set -eu
cd "$(dirname "$0")/.."

terms='\bWAL\b|ledger|postings?\b|indexer|microbatch|merger|maintenance|index node|trigram|shard|compaction|residency|partition|catalog|tier|control plane'
allow=scripts/check-public.allow

hits=$(grep -rniE "$terms" cmd README.md || true)
bad=$(printf '%s\n' "$hits" | awk -v allow="$allow" '
	BEGIN {
		while ((getline line < allow) > 0) {
			if (line == "" || line ~ /^#/) continue
			i = index(line, ":")
			n++; path[n] = substr(line, 1, i - 1); text[n] = substr(line, i + 1)
		}
	}
	$0 == "" { next }
	{
		split($0, f, ":")
		for (j = 1; j <= n; j++) if (f[1] == path[j] && index($0, text[j])) next
		print
	}')

if [ -n "$bad" ]; then
	printf '%s\n' "$bad"
	echo "Describe these in terms of the public API, or add a reviewed entry to $allow." >&2
	exit 1
fi
