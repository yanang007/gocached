// Package bearer builds the Authorization header shared by the gocached
// client and server.
package bearer

// Header is the request header carrying the token.
const Header = "Authorization"

// prefix is the scheme prefix of the header value.
const prefix = "Bearer "

// Value returns the Authorization header value for token.
func Value(token string) string { return prefix + token }
