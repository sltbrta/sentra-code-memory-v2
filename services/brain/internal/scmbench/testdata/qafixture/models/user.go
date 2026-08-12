// Package models defines the domain entities for the qafixture benchmark
// corpus. It is indexed, never built.
package models

// User is an authenticated account. The user model is referenced by the auth
// and api layers and carries role membership for authorization.
type User struct {
	ID       int
	Email    string
	Subject  string
	Roles    []string
	Disabled bool
}

// NewUser constructs an active user with the viewer role by default.
func NewUser(id int, email string) User {
	return User{ID: id, Email: email, Subject: "user:" + email, Roles: []string{"viewer"}}
}

// HasRole reports whether the user carries the named role.
func (u User) HasRole(role string) bool {
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// DisplayName returns a stable, privacy-safe display label for the user.
func (u User) DisplayName() string {
	if u.Email == "" {
		return "user"
	}
	return u.Email
}
