package auth

// Role names a permission set granted to a subject.
type Role string

// Built-in roles used by the fixture authorization layer.
const (
	RoleAdmin  Role = "admin"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// Authorize reports whether the subject's roles permit the action. Role checks
// are additive: any granting role is sufficient.
func Authorize(roles []Role, action string) bool {
	for _, r := range roles {
		if permits(r, action) {
			return true
		}
	}
	return false
}

// permits maps one role to the actions it allows.
func permits(r Role, action string) bool {
	switch r {
	case RoleAdmin:
		return true
	case RoleEditor:
		return action == "read" || action == "write"
	case RoleViewer:
		return action == "read"
	default:
		return false
	}
}

// Grant returns a new role list with the role added, deduplicated.
func Grant(roles []Role, r Role) []Role {
	for _, existing := range roles {
		if existing == r {
			return roles
		}
	}
	return append(roles, r)
}
