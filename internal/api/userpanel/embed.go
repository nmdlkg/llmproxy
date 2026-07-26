package userpanel

import _ "embed"

//go:embed user.html
var page []byte

// HTML returns the embedded user dashboard.
func HTML() []byte {
	return page
}
