package oracle

import (
	"path"
	"strings"
	"unicode"
)

// Key classes for Secret drift (D5 in docs/decisions/0001-v0.1-path.md).
const (
	// ClassCredential is a password, token or key that data or a session depends on.
	ClassCredential = "credential"
	// ClassTLS is a certificate or its private key. Charts often regenerate these on purpose
	// (genCA), and clients that pin the old CA are the ones that break.
	ClassTLS = "tls"
	// ClassOther is any other key.
	ClassOther = "other"
)

// tlsNames are the key names Kubernetes and most charts use for certificates. tls.key and ca.key
// are the private halves of those certificates, so they stay TLS; a private key under any other
// name is classed as a credential below (docs/decisions/0001-v0.1-path.md §3.5).
var tlsNames = map[string]bool{"tls.crt": true, "tls.key": true, "ca.crt": true, "ca.key": true}

// credentialWords mark a key as a credential or an encryption key when one appears as a whole word
// in its name. Whole words, not substrings, so keyspace, author and monkey are not credentials.
var credentialWords = map[string]bool{
	"password": true, "passwd": true, "pass": true, "pwd": true, "passphrase": true,
	"secret": true, "token": true, "jwt": true, "salt": true,
	"credential": true, "credentials": true, "apikey": true, "key": true, "auth": true,
	"cookie": true, "rsa": true, "connection": true, "url": true, "dsn": true,
	"dockerconfigjson": true, "dockercfg": true, "dockerconfig": true,
}

// credentialParts mark a word as a credential when it contains one, so run-together names such as
// dbpassword, htpasswd and secretkey count. Only these four: key and auth stay whole words, or
// monkey and author would be credentials.
var credentialParts = []string{"password", "passwd", "secret", "token"}

// keystoreExt are bundle formats that carry a private key, so a Secret key with one is a credential.
var keystoreExt = map[string]bool{".jks": true, ".p12": true, ".pfx": true}

// KeyClass guesses what a Secret key holds from its name alone; values are never read.
func KeyClass(key string) string {
	k := strings.ToLower(key)
	if tlsNames[k] {
		return ClassTLS
	}
	switch path.Ext(k) {
	case ".crt", ".cer":
		return ClassTLS
	case ".pem":
		// A PEM file is a certificate unless its name marks it as the private key half.
		if strings.Contains(k, "key") || strings.Contains(k, "priv") {
			return ClassCredential
		}
		return ClassTLS
	}
	if keystoreExt[path.Ext(k)] {
		return ClassCredential
	}
	// Match whole words from the original name, so camelCase (jwtSecret) splits too.
	for _, w := range words(key) {
		if isCredentialWord(w) {
			return ClassCredential
		}
	}
	return ClassOther
}

// isCredentialWord matches a word as written or without a plural s (passwords, tokens, keys), or
// one that contains a credentialParts entry.
func isCredentialWord(w string) bool {
	if credentialWords[w] || credentialWords[strings.TrimSuffix(w, "s")] {
		return true
	}
	for _, part := range credentialParts {
		if strings.Contains(w, part) {
			return true
		}
	}
	return false
}

// words splits a key name into lowercase words, on separators (-, _, ., /, space) and at camelCase
// boundaries: a lowercase letter followed by an uppercase one, and the last capital of an
// uppercase run followed by a lowercase letter (DBPassword is db, password), so credentialWords
// match as words.
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = nil
		}
	}
	rs := []rune(s)
	for i, r := range rs {
		switch {
		case r == '-' || r == '_' || r == '.' || r == '/' || r == ' ':
			flush()
		case unicode.IsUpper(r) && i > 0 && unicode.IsLower(rs[i-1]),
			unicode.IsUpper(r) && i > 0 && unicode.IsUpper(rs[i-1]) && i+1 < len(rs) && unicode.IsLower(rs[i+1]):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
	}
	flush()
	return out
}
