package cascade

import (
	"encoding/base64"
)

// base64StdEncoding returns standard base64 of s. Used to build the
// Proxy-Authorization header value for upstream CONNECT.
func base64StdEncoding(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
