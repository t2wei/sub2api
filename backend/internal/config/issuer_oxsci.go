package config

import (
	"fmt"
	"strconv"
	"strings"
)

// [OXSCI] Issuer API (P135 T13 K1): the data service that runs this binary in its
// own container opens one sub2api account per interview (balance = the budget) and
// issues short-lived keys for it. It calls a narrow API of its own, never the admin
// one. Configuration comes only from these three environment variables, which the
// data service's entrypoint puts on this process alone; with any of them missing or
// invalid the whole /api/v1/issuer group answers 404 ISSUER_DISABLED.
const (
	IssuerSecretEnv      = "SUB2API_ISSUER_SECRET"
	IssuerEmailDomainEnv = "SUB2API_ISSUER_EMAIL_DOMAIN"
	IssuerGroupIDsEnv    = "SUB2API_ISSUER_GROUP_IDS"

	// IssuerSecretMinLen is the shortest secret accepted (the data service
	// generates 64 hex characters).
	IssuerSecretMinLen = 32
)

// IssuerConfig is the issuer API's configuration, parsed once at startup.
type IssuerConfig struct {
	// Secret is the bearer secret the data service presents.
	Secret string
	// EmailDomain is the reserved domain (lowercase, no "@"): the issuer creates,
	// sees and touches only users whose email is <local>@<EmailDomain>.
	EmailDomain string
	// GroupIDs are the only groups the issuer may put on a user or bind a key to.
	GroupIDs []int64
	// DisabledReason says why the API is off; empty exactly when it is on.
	DisabledReason string
	// Misconfigured is true when some of the variables are set but the set is
	// unusable (as opposed to none set at all): worth an error line at startup.
	Misconfigured bool
}

// Enabled reports whether the issuer API serves requests.
func (c IssuerConfig) Enabled() bool { return c.DisabledReason == "" }

// AllowsGroup reports whether the issuer may use the group.
func (c IssuerConfig) AllowsGroup(groupID int64) bool {
	for _, id := range c.GroupIDs {
		if id == groupID {
			return true
		}
	}
	return false
}

// InDomain reports whether an email address is in the reserved domain
// (case-insensitive, exactly one "@", a non-empty local part).
func (c IssuerConfig) InDomain(email string) bool {
	if c.EmailDomain == "" {
		return false
	}
	email = strings.TrimSpace(email)
	at := strings.Index(email, "@")
	if at <= 0 || at != strings.LastIndex(email, "@") {
		return false
	}
	return strings.EqualFold(email[at+1:], c.EmailDomain)
}

// LoadIssuerConfig reads the three variables through getenv (os.Getenv in
// production). It never fails: an unusable set disables the API and says why.
func LoadIssuerConfig(getenv func(string) string) IssuerConfig {
	secret := strings.TrimSpace(getenv(IssuerSecretEnv))
	domain := strings.ToLower(strings.TrimSpace(getenv(IssuerEmailDomainEnv)))
	rawGroups := strings.TrimSpace(getenv(IssuerGroupIDsEnv))
	cfg := IssuerConfig{}
	if secret == "" && domain == "" && rawGroups == "" {
		cfg.DisabledReason = "not configured"
		return cfg
	}
	disable := func(format string, a ...any) IssuerConfig {
		return IssuerConfig{DisabledReason: fmt.Sprintf(format, a...), Misconfigured: true}
	}
	if secret == "" || domain == "" || rawGroups == "" {
		var missing []string
		for name, v := range map[string]string{IssuerSecretEnv: secret, IssuerEmailDomainEnv: domain, IssuerGroupIDsEnv: rawGroups} {
			if v == "" {
				missing = append(missing, name)
			}
		}
		return disable("missing %s", strings.Join(sortedStrings(missing), ", "))
	}
	if len(secret) < IssuerSecretMinLen {
		return disable("%s is shorter than %d characters", IssuerSecretEnv, IssuerSecretMinLen)
	}
	if strings.ContainsAny(domain, "@ \t") || !strings.Contains(domain, ".") {
		return disable("%s is not a domain: %q", IssuerEmailDomainEnv, domain)
	}
	seen := map[int64]bool{}
	var groups []int64
	for _, part := range strings.Split(rawGroups, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return disable("%s has a value that is not a positive group id: %q", IssuerGroupIDsEnv, part)
		}
		if !seen[id] {
			seen[id] = true
			groups = append(groups, id)
		}
	}
	if len(groups) == 0 {
		return disable("%s lists no group", IssuerGroupIDsEnv)
	}
	cfg.Secret = secret
	cfg.EmailDomain = domain
	cfg.GroupIDs = groups
	return cfg
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
