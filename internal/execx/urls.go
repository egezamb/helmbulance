package execx

import (
	"regexp"
	"strings"
)

// urlPattern leaves out trailing punctuation, so "see http://a/b?q=1, then" keeps its comma.
var urlPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s'"]*[^\s'".,;:)]`)

// RedactURLs hides the user info and the query values of every URL in s, since both often carry
// tokens: a chart URL with credentials, or a --traffic-path such as /health?token=….
func RedactURLs(s string) string {
	return urlPattern.ReplaceAllStringFunc(s, redactURL)
}

// redactURL hides each URL in a match. A single match can hold several, since helm takes a comma
// list of values files ("-f a,b"), and urlPattern keeps the comma; each comma-separated URL is
// redacted on its own, so the second one's credentials are not left behind.
func redactURL(match string) string {
	parts := strings.Split(match, ",")
	for i, p := range parts {
		if strings.Contains(p, "://") {
			parts[i] = redactOneURL(p)
		}
	}
	return strings.Join(parts, ",")
}

func redactOneURL(u string) string {
	scheme, rest, _ := strings.Cut(u, "://")
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	if at := strings.LastIndex(rest[:end], "@"); at >= 0 {
		rest = "***" + rest[at:]
	}
	q := strings.Index(rest, "?")
	if q < 0 {
		return scheme + "://" + rest
	}
	query, fragment, hasFragment := strings.Cut(rest[q+1:], "#")
	params := strings.Split(query, "&")
	for i, p := range params {
		if key, _, ok := strings.Cut(p, "="); ok {
			params[i] = key + "=***"
		}
	}
	rest = rest[:q+1] + strings.Join(params, "&")
	if hasFragment {
		rest += "#" + fragment
	}
	return scheme + "://" + rest
}
