package where

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	opEq string = "="
)

// clearGlob cleanup grafana globs like {name}
func clearGlob(query string) string {
	var builder strings.Builder
	for i := 0; i < len(query); i++ {
		if query[i] == '\\' {
			builder.WriteByte(query[i])
			if i+1 < len(query) {
				i++
				builder.WriteByte(query[i])
			}
			continue
		}
		if query[i] == '{' {
			end := strings.IndexByte(query[i+1:], '}')
			if end >= 0 {
				content := query[i+1 : i+end+1]
				if !strings.ContainsAny(content, ",.{}") && !strings.Contains(content, `\`) {
					builder.WriteString(content)
					i += end + 1
					continue
				}
			}
		} else if query[i] == '[' {
			end := strings.IndexByte(query[i+1:], ']')
			if end >= 0 {
				content := query[i+1 : i+end+1]
				if len([]rune(content)) <= 1 && !strings.Contains(content, `\`) {
					builder.WriteString(content)
					i += end + 1
					continue
				}
			}
		}
		builder.WriteByte(query[i])
	}
	return builder.String()
}

func glob(field string, query string, optionalDotAtEnd bool) string {
	if query == "*" {
		return ""
	}
	if !utf8.ValidString(query) {
		// Malformed UTF-8 cannot form a valid regular expression. Avoid
		// emitting a query that fails to compile on the server.
		return "0"
	}

	query = clearGlob(query)

	// A backslash before a glob metacharacter quotes that character;
	// otherwise it remains a literal backslash for compatibility.
	var literal strings.Builder
	var prefix strings.Builder
	wildcard := false
	for i := 0; i < len(query); i++ {
		if query[i] == '\\' && i+1 < len(query) && strings.ContainsRune(`*?{}[],.\\`, rune(query[i+1])) {
			i++
			literal.WriteByte(query[i])
			if !wildcard {
				prefix.WriteByte(query[i])
			}
			continue
		}
		if strings.ContainsRune("[]{}*?", rune(query[i])) {
			wildcard = true
		}
		literal.WriteByte(query[i])
		if !wildcard {
			prefix.WriteByte(query[i])
		}
	}

	if !wildcard {
		if optionalDotAtEnd {
			return In(field, []string{literal.String(), literal.String() + "."})
		} else {
			return Eq(field, literal.String())
		}
	}

	// before any wildcard symbol
	simplePrefix := prefix.String()

	// Q() replaces \ with \\, so using \. does not work here.
	// work around with [.]
	postfix := `$`
	if optionalDotAtEnd {
		postfix = `[.]?$`
	}

	if simplePrefix == "" {
		return fmt.Sprintf("match(%s, %s)", field, quote(`^`+GlobToRegexp(query)+postfix))
	}

	return fmt.Sprintf("%s AND match(%s, %s)",
		HasPrefix(field, simplePrefix),
		field, quote(`^`+GlobToRegexp(query)+postfix),
	)
}

// Glob ...
func Glob(field string, query string) string {
	return glob(field, query, false)
}

// TreeGlob ...
func TreeGlob(field string, query string) string {
	return glob(field, query, true)
}

func ConcatMatchKV(key, value string) string {
	if value == "" {
		return key + opEq + `\\%`
	}
	startLine := value[0] == '^'
	endLine := value[len(value)-1] == '$'
	if startLine && endLine {
		return key + opEq + value[1:]
	} else if startLine {
		return key + opEq + value[1:] + "\\\\%"
	}
	return key + opEq + "\\\\%" + value
}

func Match(field string, key, value string) string {
	if !utf8.ValidString(key) || !utf8.ValidString(value) {
		return "0"
	}
	if len(value) == 0 || value == "*" {
		return Like(field, likeEscape(key+"=")+"%")
	}
	// An exact match needs no regex, even when the key includes characters
	// special to regular expressions.
	if strings.HasPrefix(value, "^") && strings.HasSuffix(value, "$") &&
		len(value) >= 2 && regexp.QuoteMeta(value[1:len(value)-1]) == value[1:len(value)-1] {
		return Eq(field, key+"="+value[1:len(value)-1])
	}
	expr := ConcatMatchKV(key, value)
	simplePrefix := NonRegexpPrefix(expr)
	if strings.Contains(value, "|") {
		// Alternatives are scoped to the value by quoteRegex; neither
		// alternative is guaranteed to start with the other one's prefix.
		simplePrefix = key + opEq
	}
	quotedRegexp, valid := quoteRegex(key, value)
	if !valid {
		// A malformed regex must not become a different, literal search or
		// cause ClickHouse to reject the entire query.
		return "0"
	}
	regexSQL := fmt.Sprintf("match(%s, %s)", field, quotedRegexp)
	// A regex prefix extracted from a key containing metacharacters, or
	// from an unanchored value, may exclude valid matches. In those cases
	// rely on the regex alone rather than an unsafe LIKE prefilter.
	if !strings.HasPrefix(value, "^") || len(simplePrefix) < len(key)+1 {
		return regexSQL
	}
	return HasPrefix(field, simplePrefix) + " AND " + regexSQL
}
