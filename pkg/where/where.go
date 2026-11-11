package where

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unsafe"

	"github.com/lomik/pluto/pkg/errs"
)

func unsafeString(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

// workaraund for Grafana multi-value variables, expand S{a,b,c}E to [SaE,SbE,ScE]
func GlobExpandSimple(value, prefix string, result *[]string) error {
	if len(value) == 0 {
		// we at the end of glob
		*result = append(*result, prefix)
		return nil
	}

	start := strings.IndexAny(value, "{}")
	if start == -1 {
		*result = append(*result, prefix+value)
	} else {
		end := strings.Index(value[start:], "}")
		if end <= 1 {
			return errs.NewErrorWithCode("malformed glob: "+value, http.StatusBadRequest)
		}
		if end == -1 || strings.IndexAny(value[start+1:start+end], "{}") != -1 {
			return errs.NewErrorWithCode("malformed glob: "+value, http.StatusBadRequest)
		}
		if start > 0 {
			prefix = prefix + value[0:start]
		}
		g := value[start+1 : start+end]
		values := strings.Split(g, ",")
		var postfix string
		if end+start-1 < len(value) {
			postfix = value[start+end+1:]
		}
		for _, v := range values {
			if err := GlobExpandSimple(postfix, prefix+v, result); err != nil {
				return err
			}
		}
	}

	return nil
}

func GlobToRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		switch g[i] {
		case '*':
			b.WriteString("([^.]*?)")
		case '?':
			b.WriteString("[^.]")
		case '.':
			b.WriteString("[.]")
		case '$':
			b.WriteString("[$]")
		case '\\':
			if i+1 < len(g) && strings.ContainsRune(`*?{}[],.\\`, rune(g[i+1])) {
				i++
				b.WriteString(regexp.QuoteMeta(g[i : i+1]))
			} else {
				b.WriteString(`\\`)
			}
		case '[':
			end := strings.IndexByte(g[i+1:], ']')
			if end >= 0 {
				class := g[i : i+end+2]
				if _, err := regexp.Compile(class); err == nil {
					b.WriteString(class)
					i += end + 1
					continue
				}
			}
			b.WriteString(`\[`)
		case ']':
			b.WriteString(`\]`)
		case '{':
			end := strings.IndexByte(g[i+1:], '}')
			if end >= 0 && strings.Contains(g[i+1:i+end+1], ",") && !strings.ContainsAny(g[i+1:i+end+1], "{}\\") {
				b.WriteByte('(')
				for j, choice := range strings.Split(g[i+1:i+end+1], ",") {
					if j > 0 {
						b.WriteByte('|')
					}
					b.WriteString(GlobToRegexp(choice))
				}
				b.WriteByte(')')
				i += end + 1
			} else {
				b.WriteString(`\{`)
			}
		case '}':
			b.WriteString(`\}`)
		default:
			b.WriteString(regexp.QuoteMeta(g[i : i+1]))
		}
	}
	return b.String()
}

func HasWildcard(target string) bool {
	return strings.IndexAny(target, "[]{}*?") > -1
}

func IndexLastWildcard(target string) int {
	return strings.LastIndexAny(target, "[]{}*?")
}

func IndexWildcard(target string) int {
	return strings.IndexAny(target, "[]{}*?")
}

func NonRegexpPrefix(expr string) string {
	// Match scopes alternatives to the value, so a | in the value can
	// invalidate a prefix discovered before another regex metacharacter.
	// Preserve the key/equals portion in that case.
	if strings.Contains(expr, "|") {
		eq := strings.IndexByte(expr, '=')
		if eq >= 0 {
			if eq+1 < len(expr) && expr[eq+1] == '~' {
				eq++
			}
			return expr[:eq+1]
		}
		return ""
	}
	for i := 0; i < len(expr); i++ {
		if strings.ContainsRune(`\\.+*?()[]{}^$`, rune(expr[i])) {
			return expr[:i]
		}
	}
	return expr
}

func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return s
}

func likeEscape(s string) string {
	// LIKE consumes one level of escaping after the SQL string literal is
	// decoded. Original backslashes need escaping at both levels; wildcard
	// escapes only need escaping for the SQL string literal.
	s = strings.ReplaceAll(s, `\`, `\\\\`)
	s = strings.ReplaceAll(s, `_`, `\\_`)
	s = strings.ReplaceAll(s, `%`, `\\%`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return s
}

func quote(value interface{}) string {
	switch v := value.(type) {
	case int, uint32, int64:
		return fmt.Sprintf("%#v", v)
	case string:
		return fmt.Sprintf("'%s'", escape(v))
	case []byte:
		return fmt.Sprintf("'%s'", escape(unsafeString(v)))
	default:
		panic("not implemented")
	}
}

func quoteRegex(key, value string) (string, bool) {
	pattern := `^` + regexp.QuoteMeta(key) + opEq
	if strings.HasPrefix(value, "^") {
		value = value[1:]
	} else {
		pattern += `.*`
	}
	if strings.Contains(value, "|") {
		value = "(" + value + ")"
	}
	if _, err := regexp.Compile(pattern + value); err != nil {
		return "", false
	}
	return quote(pattern + value), true
}

func Like(field, s string) string {
	return fmt.Sprintf("%s LIKE '%s'", field, s)
}

func Eq(field, value interface{}) string {
	return fmt.Sprintf("%s=%s", field, quote(value))
}

func ArrayElementEq(field string, element string, value interface{}) string {
	return fmt.Sprintf("arrayElement(`%s`,%s)=%s", field, quote(element), quote(value))
}

func HasPrefix(field, prefix string) string {
	return fmt.Sprintf("%s LIKE '%s%%'", field, likeEscape(prefix))
}

func HasPrefixAndNotEq(field, prefix string) string {
	return fmt.Sprintf("%s LIKE '%s_%%'", field, likeEscape(prefix))
}

func HasPrefixBytes(field, prefix []byte) string {
	return fmt.Sprintf("%s LIKE '%s%%'", field, likeEscape(unsafeString(prefix)))
}

func In(field string, list []string) string {
	if len(list) == 1 {
		return Eq(field, list[0])
	}

	var buf strings.Builder
	buf.WriteString(field)
	buf.WriteString(" IN (")
	for i, v := range list {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(quote(v))
	}
	buf.WriteByte(')')
	return buf.String()
}

func InTable(field string, table string) string {
	return fmt.Sprintf("%s in %s", field, table)
}

func BetweenInt64(field string, from int64, until int64) string {
	return fmt.Sprintf("%s >= %d AND %s <= %d", field, from, field, until)
}

type Where struct {
	where string
}

func New() *Where {
	return &Where{}
}

func (w *Where) And(exp string) {
	if exp == "" {
		return
	}
	if w.where != "" {
		w.where = fmt.Sprintf("(%s) AND (%s)", w.where, exp)
	} else {
		w.where = exp
	}
}

func (w *Where) Or(exp string) {
	if exp == "" {
		return
	}
	if w.where != "" {
		w.where = fmt.Sprintf("(%s) OR (%s)", w.where, exp)
	} else {
		w.where = exp
	}
}

func (w *Where) Andf(format string, obj ...interface{}) {
	w.And(fmt.Sprintf(format, obj...))
}

func (w *Where) String() string {
	return w.where
}

func (w *Where) SQL() string {
	if w.where == "" {
		return ""
	}
	return "WHERE " + w.where
}

func (w *Where) PreWhereSQL() string {
	if w.where == "" {
		return ""
	}
	return "PREWHERE " + w.where
}
