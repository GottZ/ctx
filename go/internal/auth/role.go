package auth

// Role is a key's tenant-role: its authority WITHIN its owning tenant
// (Modell C, 059). It is ORTHOGONAL to the server-global is_admin tier
// (M052) — a key may be a server-admin (IsAdmin) regardless of its tenant
// Role, and a tenant Role grants nothing outside its own tenant.
//
// The domain is byte-identical to the DB CHECK on
// context_api_keys.tenant_role (059:118-119, K4): a Go constant that drifts
// from the CHECK would pass one layer and fail the other. owner administers +
// delegates (appoints admins, OE-2), admin administers, member uses
// (059 header). Verified against the live CHECK in role_integration_test.go.
type Role string

const (
	RoleOwner  Role = "owner"  // tenant owner: administer + delegate (OE-2)
	RoleAdmin  Role = "admin"  // tenant admin: administer, no delegation
	RoleMember Role = "member" // default tier: use only (059 DEFAULT, fail-closed)
)

// administers reports whether a tenant Role carries tenant-administration
// authority. owner and admin both administer; member — and any unknown or
// empty value, including the Go zero value "" — does not. fail-closed: a
// forgotten or drifted role never silently grants admin rights.
func (r Role) administers() bool {
	return r == RoleOwner || r == RoleAdmin
}

// delegates reports whether a tenant Role may DELEGATE roles — appoint or
// revoke admins and mutate owner keys (OE-2). ONLY owner delegates; admin
// administers (administers() == true) but does NOT delegate (059 header:
// "admin: administer, no delegation"). This is strictly narrower than
// administers(): owner→true, admin→false, member→false, and any unknown or
// empty value, including the Go zero value "", → false (fail-closed mirror).
func (r Role) delegates() bool {
	return r == RoleOwner
}

// ValidRole reports whether s is a member of the 059 CHECK domain on
// context_api_keys.tenant_role ('owner'|'admin'|'member'). It is the Go-side
// gate in front of the 23514 CHECK backstop, so a bad role yields a clean 400
// rather than a raw SQL constraint error. "" / unknown → false (fail-closed).
func ValidRole(s string) bool {
	return s == string(RoleOwner) || s == string(RoleAdmin) || s == string(RoleMember)
}

// IsServerAdmin reports the M052 top tier: a valid key with the server-global
// admin bit. server-admins act across ALL tenants and ALL actions. A nil or
// invalid receiver is fail-closed.
func (ar *AuthResult) IsServerAdmin() bool {
	return ar != nil && ar.IsValid && ar.IsAdmin
}

// IsTenantAdminOf reports whether this key may administer the given tenant.
// server-admins administer every (non-empty) tenant; tenant-admins (owner or
// admin role) administer only their OWN tenant. fail-closed on every edge:
// nil/invalid receiver, an empty target tenant (for ALL tiers, server-admin
// included), an empty caller tenant, a tenant mismatch, or an unknown/member
// role all yield false.
//
// The empty-target guard stands BEFORE the server-admin short-circuit by
// design (design/05 §4.2 Rev. 2): "empty target ⇒ no access" is a true
// invariant for every tier — defense-in-depth against a call site that
// forgets to set the target tenant.
func (ar *AuthResult) IsTenantAdminOf(tenant string) bool {
	if ar == nil || !ar.IsValid {
		return false
	}
	if tenant == "" {
		return false // fail-closed: empty target tenant — applies to server-admin too
	}
	if ar.IsAdmin {
		return true // server-admin overrides (over a non-empty target)
	}
	if ar.TenantID == "" {
		return false // fail-closed: caller without a tenant
	}
	return ar.TenantID == tenant && ar.TenantRole.administers()
}
