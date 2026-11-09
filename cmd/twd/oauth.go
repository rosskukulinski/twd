package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/rosskukulinski/twd/internal/policy"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/mgmt"
	"github.com/rosskukulinski/twd/netd"
)

// OAuth mint mode (M12, the default for self-hosted installs): a Tailscale
// OAuth client scoped to auth_keys with tag tag:twd (the same mechanism as
// the Kubernetes operator and the GitHub Action). Its secret never expires
// and stays in a 0600 file on the host; `twd up` and twd-mgmt read it at
// use and exchange it for short-lived access tokens. There is no browser
// login, no Funnel, no public issuer and no HTTPS requirement: the OAuth
// client mints mgmt's own node key (tag:twd-mgmt), the host's node key
// (tag:twd-host) and every object's key, all tags tag:twd owns.

const consoleOAuthURL = "https://login.tailscale.com/admin/settings/oauth"

// oauthConsoleSteps are the admin-console steps to create the client.
func oauthConsoleSteps() []string {
	return []string{
		"1. Admin console › Settings › OAuth clients › Generate OAuth client (" + consoleOAuthURL + ")",
		"2. Scopes: Auth Keys › Write, tag tag:twd   (required: mints mgmt's, the host's and every object's node key)",
		"   optional: Devices › Core › Write, tag tag:twd   (kill also deletes the device; without it the device stays listed)",
		"   optional: Policy File › Read   (deploy preflight and spawn checks read the tailnet policy)",
		"3. Save the client ID and secret to a file only you can read:",
		"     printf 'TS_OAUTH_CLIENT_ID=%s\\nTS_OAUTH_CLIENT_SECRET=%s\\n' <id> <tskey-client-…> > ~/.config/twd/oauth.env && chmod 600 ~/.config/twd/oauth.env",
		"4. Policy (Access controls), before twd up: tag:twd must own tag:twd-mgmt and tag:twd-host (snippet below)",
	}
}

// tagOwnersNeeded is what the mint credential (tag:twd) needs to mint
// mgmt's and the host's node keys: tag:twd among their owners.
func tagOwnersNeeded() policy.Snippet {
	return policy.Snippet{TagOwners: map[string][]string{mgmt.MgmtTag: {"tag:twd"}, mgmt.HostTag: {"tag:twd"}}}
}

// tagOwnersPart is the tagOwners section of the operator snippet.
func tagOwnersPart() policy.Snippet {
	return policy.Snippet{TagOwners: operatorSnippet().TagOwners}
}

// snippetLines renders s indented for the terminal.
func snippetLines(s policy.Snippet) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(s.HuJSON()), "\n") {
		out = append(out, "    "+l)
	}
	return out
}

// policyMissingTagOwners is the typed error for "the control plane refused
// (or would refuse) to mint keys for twd's own tags": before any login URL
// or mint, with the exact snippet.
func policyMissingTagOwners(why string, cause error) *cliErr {
	lines := []string{why}
	if cause != nil {
		lines = append(lines, "control plane: "+netd.Redact(cause.Error()))
	}
	fix := append([]string{"add to your tailnet policy (admin console › Access controls), then run twd up again (nothing was minted):"}, snippetLines(tagOwnersPart())...)
	return fail("policy_missing_tag_owners", "the tailnet policy does not let tag:twd own "+mgmt.MgmtTag+" and "+mgmt.HostTag, lines...).withFix(fix...)
}

// mintModeRequired is `twd up` with no mint mode on a fresh state dir: the
// three options, recommended first, instead of starting a browser flow.
func mintModeRequired() *cliErr {
	lines := []string{
		"twd mints a Tailscale auth key for every node it runs (mgmt, this host, each object), so it needs one credential. Pick one:",
		"",
		"  1. OAuth client (recommended): no browser, no Funnel, no HTTPS needed; the secret stays in a 0600 file on this machine",
	}
	for _, s := range oauthConsoleSteps() {
		lines = append(lines, "       "+s)
	}
	lines = append(lines, "     twd up --oauth-client-file ~/.config/twd/oauth.env [--service] [--yes]",
		"",
		"  2. Workload identity federation (no secret at all, for hosted / multi-tenant mgmt): browser login, Funnel + HTTPS,",
		"     mgmt serves a public OIDC issuer and you create a trust credential for it:",
		"     twd up --wif",
		"",
		"  3. API key (explicit fallback; it expires within 90 days and can do everything the key's owner can):",
		"     twd up --tailnet <tailnet>.ts.net --api-key-file <file with TS_API_KEY=…>",
		"",
		"policy for 1 (and 2, plus nodeAttrs funnel on "+mgmt.MgmtTag+"):")
	lines = append(lines, snippetLines(operatorSnippet())...)
	return fail("mint_mode_required", "choose how twd mints node keys", lines...).
		withFix("twd up --oauth-client-file <file>   (recommended)", "twd up --wif", "twd up --tailnet <tailnet> --api-key-file <file>")
}

// oauthPreflight runs before anything is minted: the client exchanges, the
// tailnet is named (devices endpoint, if the client may list devices),
// and, if the client may read the policy, tag ownership is verified.
// readable reports whether the policy could be read; pol is it then.
func oauthPreflight(ctx context.Context, api *tsapi.Client, src *tsapi.OAuthSource, clientID, file string, out io.Writer) (tailnet string, pol []byte, readable bool, err error) {
	if _, err := src.Token(ctx); err != nil {
		return "", nil, false, fail("oauth_exchange_failed", "Tailscale refused the OAuth client "+clientID+" from "+file, netd.Redact(err.Error())).
			withFix("check TS_OAUTH_CLIENT_SECRET in "+file+" (admin console › Settings › OAuth clients); a revoked client must be regenerated",
				"the client needs the Auth Keys (write) scope with tag tag:twd")
	}
	if api.Tailnet == "-" {
		if tn, err := api.DiscoverTailnet(ctx); err == nil {
			tailnet = tn
		}
	}
	p, _, perr := api.GetPolicyHuJSON(ctx)
	if perr != nil {
		fmt.Fprintf(out, "› policy   the OAuth client can't read the policy (%s); tag ownership is checked by the first mint instead\n", firstLine(netd.Redact(perr.Error())))
		return tailnet, nil, false, nil
	}
	miss, merr := policy.Missing(p, tagOwnersNeeded())
	if merr != nil {
		return tailnet, p, true, fail("bad_request", "can't parse the tailnet policy", merr.Error())
	}
	if !miss.Empty() {
		return tailnet, p, true, policyMissingTagOwners("the policy (read with the OAuth client) lacks tag:twd as an owner of: "+strings.Join(slices.Sorted(maps.Keys(miss.TagOwners)), ", "), nil)
	}
	return tailnet, p, true, nil
}

// mintNodeKey mints a single-use, pre-authorized node key for twd's own
// nodes with the OAuth client or API key, translating a tag refusal.
func mintNodeKey(ctx context.Context, api *tsapi.Client, tag, desc string) (string, error) {
	k, err := api.CreateKey(ctx, tsapi.KeyOptions{Tags: []string{tag}, Preauthorized: true, ExpirySeconds: 300, Description: desc})
	if err != nil {
		if tsapi.IsTagsNotPermitted(err) {
			return "", policyMissingTagOwners("the control plane refused a "+tag+" key for the credential's tag", err)
		}
		return "", fail("bootstrap_failed", "could not mint "+tag+"'s node key", netd.Redact(err.Error()))
	}
	return k.Secret, nil
}
