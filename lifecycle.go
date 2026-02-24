package main

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// SiteStatus represents the lifecycle state of a site.
// Sites move through defined states via explicit, validated transitions.
type SiteStatus string

const (
	SiteCreated          SiteStatus = "CREATED"
	SiteProvisioning     SiteStatus = "PROVISIONING"
	SiteActive           SiteStatus = "ACTIVE"
	SiteDomainPending    SiteStatus = "DOMAIN_PENDING"
	SiteDomainValidating SiteStatus = "DOMAIN_VALIDATING"
	SiteDomainRouting    SiteStatus = "DOMAIN_ROUTING"
	SiteDomainActive     SiteStatus = "DOMAIN_ACTIVE"
	SiteDomainRemoving   SiteStatus = "DOMAIN_REMOVING"
	SiteDestroying       SiteStatus = "DESTROYING"
	SiteDestroyed        SiteStatus = "DESTROYED"
	SiteFailed           SiteStatus = "FAILED"
)

// allowedTransitions defines the legal state machine edges.
// No transition outside this map is permitted.
var allowedTransitions = map[SiteStatus][]SiteStatus{
	SiteCreated:          {SiteProvisioning},
	SiteProvisioning:     {SiteActive, SiteFailed},
	SiteActive:           {SiteDomainPending, SiteDestroying},
	SiteDomainPending:    {SiteDomainValidating, SiteActive},
	SiteDomainValidating: {SiteDomainRouting, SiteDomainPending, SiteActive},
	SiteDomainRouting:    {SiteDomainActive, SiteActive},
	SiteDomainActive:     {SiteDomainRemoving, SiteDestroying},
	SiteDomainRemoving:   {SiteActive, SiteFailed},
	SiteDestroying:       {SiteDestroyed, SiteFailed},
	SiteFailed:           {SiteProvisioning, SiteDestroying},
}

// CanTransitionTo checks whether a transition from this status to the target is allowed.
func (from SiteStatus) CanTransitionTo(to SiteStatus) bool {
	for _, allowed := range allowedTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// String returns the string representation.
func (s SiteStatus) String() string {
	return string(s)
}

// IsValid returns whether this is a recognized status value.
func (s SiteStatus) IsValid() bool {
	_, ok := allowedTransitions[s]
	return ok
}

// IsTerminal returns whether this status represents a final state
// that requires explicit operator action to leave.
func (s SiteStatus) IsTerminal() bool {
	return s == SiteDestroyed || s == SiteFailed
}

// AllowsCustomDomain returns whether a custom domain can be attached in this state.
func (s SiteStatus) AllowsCustomDomain() bool {
	return s == SiteActive
}

// AllowsDestroy returns whether the site can be destroyed from this state.
func (s SiteStatus) AllowsDestroy() bool {
	return s == SiteActive || s == SiteDomainActive || s == SiteFailed
}

// ── Domain Validation ────────────────────────────────────────────────────────

var domainRegexp = regexp.MustCompile(
	`^([a-zA-Z0-9]([a-zA-Z0-9\-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`,
)

// ValidateDomainFormat checks basic domain name format validity.
func ValidateDomainFormat(domain string) error {
	if domain == "" {
		return fmt.Errorf("domain cannot be empty")
	}
	if len(domain) > 253 {
		return fmt.Errorf("domain too long (max 253 characters)")
	}
	if strings.HasPrefix(domain, "*.") {
		return fmt.Errorf("wildcard domains are not supported")
	}
	if !domainRegexp.MatchString(domain) {
		return fmt.Errorf("invalid domain format: %s", domain)
	}
	return nil
}

// ValidateDomainNotBase rejects domains that are or overlap with the base domain.
func ValidateDomainNotBase(domain, baseDomain string) error {
	if strings.HasSuffix(domain, "."+baseDomain) || domain == baseDomain {
		return fmt.Errorf("cannot use %s as custom domain (conflicts with base domain %s)", domain, baseDomain)
	}
	return nil
}

// ValidateCustomDomain runs all synchronous domain validations.
func ValidateCustomDomain(domain, baseDomain string) error {
	if err := ValidateDomainFormat(domain); err != nil {
		return err
	}
	return ValidateDomainNotBase(domain, baseDomain)
}

// ValidateDomainDNS checks that the domain resolves (async-safe, used by reconciler).
func ValidateDomainDNS(domain string) error {
	addrs, err := net.LookupHost(domain)
	if err != nil {
		return fmt.Errorf("domain %s does not resolve: %w", domain, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("domain %s has no DNS records", domain)
	}
	return nil
}

// ValidateDomainPointsToIngress verifies the domain's A record resolves to the
// expected public ingress IP (the VPS TCP forwarder). Custom domains must point
// here before Caddy can obtain a TLS certificate for them.
func ValidateDomainPointsToIngress(domain, expectedIP string) error {
	addrs, err := net.LookupHost(domain)
	if err != nil {
		return fmt.Errorf("domain %s does not resolve: %w", domain, err)
	}
	for _, addr := range addrs {
		if addr == expectedIP {
			return nil
		}
	}
	return fmt.Errorf("domain %s does not point to %s (resolved: %s) — set an A record to %s first",
		domain, expectedIP, strings.Join(addrs, ", "), expectedIP)
}
