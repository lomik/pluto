package where

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/lomik/pluto/pkg/errs"
)

// sqlString decodes the backslash-escaped SQL string literals emitted by this
// package. It also rejects any unquoted SQL after a generated predicate.
func sqlString(t *testing.T, input string) (string, string) {
	t.Helper()
	if !strings.HasPrefix(input, "'") {
		t.Fatalf("expected SQL string literal, got %q", input)
	}
	var value strings.Builder
	for i := 1; i < len(input); i++ {
		switch input[i] {
		case '\\':
			i++
			if i == len(input) {
				t.Fatalf("unterminated SQL escape in %q", input)
			}
			value.WriteByte(input[i])
		case '\'':
			return value.String(), input[i+1:]
		default:
			value.WriteByte(input[i])
		}
	}
	t.Fatalf("unterminated SQL string in %q", input)
	return "", ""
}

func likeMatches(t *testing.T, pattern, value string) bool {
	t.Helper()
	var re strings.Builder
	re.WriteByte('^')
	escaped := false
	for _, r := range pattern {
		switch {
		case escaped:
			re.WriteString(regexp.QuoteMeta(string(r)))
			escaped = false
		case r == '\\':
			escaped = true
		case r == '%':
			re.WriteString("(?s:.*)")
		case r == '_':
			re.WriteString("(?s:.)")
		default:
			re.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	if escaped {
		t.Fatalf("trailing LIKE escape in %q", pattern)
	}
	re.WriteByte('$')
	return regexp.MustCompile(re.String()).MatchString(value)
}

// predicateMatches interprets the small, documented subset of SQL emitted by
// Glob/TreeGlob/Match, without using a ClickHouse server. Rejecting trailing
// SQL also catches broken quoting and SQL injection in the generated strings.
func predicateMatches(t *testing.T, sql, value string) bool {
	t.Helper()
	if sql == "" {
		return true // the existing special case for the catch-all glob "*"
	}
	if sql == "0" {
		return false // deliberately invalid regex/UTF-8
	}
	const field = "metric"
	if strings.HasPrefix(sql, "match("+field+", ") {
		pattern, rest := sqlString(t, strings.TrimPrefix(sql, "match("+field+", "))
		if rest != ")" {
			t.Fatalf("unexpected SQL after match: %q", rest)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Fatalf("invalid generated regex %q: %v", pattern, err)
		}
		return re.MatchString(value)
	}
	if strings.HasPrefix(sql, field+" LIKE ") {
		pattern, rest := sqlString(t, strings.TrimPrefix(sql, field+" LIKE "))
		if rest != "" && !strings.HasPrefix(rest, " AND match(") {
			t.Fatalf("unexpected SQL after LIKE: %q", rest)
		}
		matched := likeMatches(t, pattern, value)
		if rest != "" {
			return matched && predicateMatches(t, strings.TrimPrefix(rest, " AND "), value)
		}
		return matched
	}
	if strings.HasPrefix(sql, field+"=") {
		literal, rest := sqlString(t, strings.TrimPrefix(sql, field+"="))
		if rest != "" {
			t.Fatalf("unexpected SQL after equality: %q", rest)
		}
		return value == literal
	}
	if strings.HasPrefix(sql, field+" IN (") {
		first, rest := sqlString(t, strings.TrimPrefix(sql, field+" IN ("))
		if !strings.HasPrefix(rest, ",") {
			t.Fatalf("unexpected IN clause: %q", sql)
		}
		second, rest := sqlString(t, strings.TrimPrefix(rest, ","))
		if rest != ")" {
			t.Fatalf("unexpected SQL after IN clause: %q", rest)
		}
		return value == first || value == second
	}
	t.Fatalf("unrecognised generated SQL: %q", sql)
	return false
}

func TestGlobPredicates(t *testing.T) {
	tests := []struct {
		name, query string
		tree        bool
		matches     []string
		rejects     []string
	}{
		{"literal", "foo", false, []string{"foo"}, []string{"foo.", "foobar"}},
		{"all", "*", false, []string{"", "foo.bar"}, nil},
		{"star does not cross dot", "foo*", false, []string{"foo", "foobar"}, []string{"foo.bar", "foox.bar"}},
		{"star in segment", "foo.*.bar", false, []string{"foo.x.bar", "foo..bar"}, []string{"foo.x.y.bar"}},
		{"question mark", "foo?", false, []string{"foox"}, []string{"foo", "foo.x"}},
		{"choice", "foo.{bar,baz}", false, []string{"foo.bar", "foo.baz"}, []string{"foo.qux"}},
		{"choice with wildcard", "foo.{bar*,baz?}", false, []string{"foo.barx", "foo.bazx"}, []string{"foo.bar.x", "foo.baz"}},
		{"simple choice", "foo.{bar}", false, []string{"foo.bar"}, []string{"foo.baz"}},
		{"single char class", "foo.[x]", false, []string{"foo.x"}, []string{"foo.y"}},
		{"escaped wildcard", `foo\*bar*`, false, []string{"foo*bar", "foo*barX"}, []string{"foobarX", "fooxbarX"}},
		{"escaped question", `foo\?bar`, false, []string{"foo?bar"}, []string{"fooxbar"}},
		{"escaped bracket", `foo\[bar*`, false, []string{"foo[bar", "foo[barX"}, []string{"fooXbarX"}},
		{"escaped dot", `foo\.bar*`, false, []string{"foo.bar", "foo.barX"}, []string{"fooXbarX"}},
		{"dot class", "foo[.]bar", false, []string{"foo.bar"}, []string{"fooXbar"}},
		{"range", "foo.[a-c]", false, []string{"foo.a", "foo.c"}, []string{"foo.d"}},
		{"literal regex operators", "foo+(a|b),c*", false, []string{"foo+(a|b),c", "foo+(a|b),cx"}, []string{"foooa,cx", "foo+(a|b),cx.y"}},
		{"literal caret and dollar", "foo^$?", false, []string{"foo^$x"}, []string{"foox", "foo^$x.y"}},
		{"literal backslash", "foo\\bar*", false, []string{"foo\\barX"}, []string{"foobarX", "foo\\bar.X"}},
		{"literal glob empty", "", false, []string{""}, []string{"a"}},
		{"unclosed class", "foo[", false, []string{"foo["}, []string{"foo", "foox"}},
		{"unclosed choice", "foo{bar", false, []string{"foo{bar"}, []string{"foobar"}},
		{"unmatched class closer", "foo]*", false, []string{"foo]x"}, []string{"foox"}},
		{"unmatched closer", "foo}*", false, []string{"foo}x"}, []string{"foox", "foo}x.y"}},
		{"escaped SQL literal", `a%'_\\*`, false, []string{"a%'_\\x"}, []string{"ax'_\\x", "a%'_\\x.y"}},
		{"tree literal", "foo", true, []string{"foo", "foo."}, []string{"foobar", "foo.."}},
		{"tree star", "foo*", true, []string{"foo", "foo.", "foobar."}, []string{"foo.bar", "foo.bar."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			predicate := Glob("metric", tt.query)
			if tt.tree {
				predicate = TreeGlob("metric", tt.query)
			}
			for _, value := range tt.matches {
				if !predicateMatches(t, predicate, value) {
					t.Errorf("%q must match %q; generated %q", tt.query, value, predicate)
				}
			}
			for _, value := range tt.rejects {
				if predicateMatches(t, predicate, value) {
					t.Errorf("%q must reject %q; generated %q", tt.query, value, predicate)
				}
			}
		})
	}
}

func TestMatchPredicates(t *testing.T) {
	tests := []struct {
		name, key, pattern string
		matches            []string
		rejects            []string
	}{
		{"exact", "host", "^foo$", []string{"host=foo"}, []string{"host=foobar", "other=foo"}},
		{"empty exact", "host", "^$", []string{"host="}, []string{"host=foo", "host=other"}},
		{"unanchored plain", "host", "foo", []string{"host=foo", "host=barfoo"}, []string{"host=bar"}},
		{"anchored prefix", "host", "^foo", []string{"host=foo", "host=foobar"}, []string{"host=barfoo"}},
		{"substring", "host", "foo", []string{"host=foo", "host=barfoo"}, []string{"host=bar"}},
		{"alternation after metachar", "host", "^foo.bar|baz$", []string{"host=fooXbar", "host=baz"}, []string{"host=foo"}},
		{"alternation with equals", "host", "^foo=bar|baz$", []string{"host=foo=bar", "host=baz"}, []string{"host=foo", "host=foo=qux"}},
		{"empty value", "host", "", []string{"host=", "host=foo"}, []string{"hostname=foo", "host"}},
		{"any value", "host", "*", []string{"host=", "host=foo"}, []string{"host"}},
		{"literal key regex chars", "a.b|c[0]", "^foo$", []string{"a.b|c[0]=foo"}, []string{"axb|c[0]=foo", "a.b|c0=foo"}},
		{"key SQL and LIKE escaping", "a%'_\\' OR 1=1 --", "", []string{"a%'_\\' OR 1=1 --=ok"}, []string{"a%'x\\' OR 1=1 --=ok", "anything=ok"}},
		{"regex key SQL injection", "a' OR 1=1 --", "^f.*$", []string{"a' OR 1=1 --=foo"}, []string{"another=foo"}},
		{"regex value SQL injection", "host", "^f'\\..*$", []string{"host=f'.abc"}, []string{"host=f'abc"}},
		{"invalid expression", "host", "^foo[", nil, []string{"host=foo[", "host=foo", "host=bar"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			predicate := Match("metric", tt.key, tt.pattern)
			if tt.name == "invalid expression" && predicate != "0" {
				t.Errorf("invalid regexp must fail closed, got %q", predicate)
			}
			for _, value := range tt.matches {
				if !predicateMatches(t, predicate, value) {
					t.Errorf("%q must match %q; generated %q", tt.pattern, value, predicate)
				}
			}
			for _, value := range tt.rejects {
				if predicateMatches(t, predicate, value) {
					t.Errorf("%q must reject %q; generated %q", tt.pattern, value, predicate)
				}
			}
		})
	}
}

func TestSQLPrefixEscapes(t *testing.T) {
	tests := []struct {
		prefix  string
		matches []string
		rejects []string
	}{
		{"", []string{"", "anything"}, nil},
		{"a%_\\'", []string{"a%_\\'", "a%_\\'suffix"}, []string{"ax_\\'suffix", "a%z\\'suffix", "a%_x'suffix", "a%_\\suffix"}},
	}
	for _, tt := range tests {
		for _, value := range tt.matches {
			if !predicateMatches(t, HasPrefix("metric", tt.prefix), value) || !predicateMatches(t, HasPrefixBytes([]byte("metric"), []byte(tt.prefix)), value) {
				t.Errorf("prefix %q should match %q", tt.prefix, value)
			}
		}
		for _, value := range tt.rejects {
			if predicateMatches(t, HasPrefix("metric", tt.prefix), value) || predicateMatches(t, HasPrefixBytes([]byte("metric"), []byte(tt.prefix)), value) {
				t.Errorf("prefix %q should reject %q", tt.prefix, value)
			}
		}
	}
	if predicateMatches(t, HasPrefixAndNotEq("metric", "a%_\\'"), "a%_\\'") || !predicateMatches(t, HasPrefixAndNotEq("metric", "a%_\\'"), "a%_\\'x") {
		t.Fatal("HasPrefixAndNotEq must require one additional character")
	}
}

func TestLikeEscapeSQLAndPatternLevels(t *testing.T) {
	// After SQL string decoding the pattern must still carry backslashes
	// before LIKE wildcards. This protects against applying only one of the
	// two independent layers of escaping.
	want := `a\%\_\\%`
	literal, rest := sqlString(t, strings.TrimPrefix(HasPrefix("metric", `a%_\`), "metric LIKE "))
	if rest != "" || literal != want {
		t.Fatalf("decoded LIKE pattern = %q (remaining SQL %q), want %q", literal, rest, want)
	}
}

func TestGlobExpandSimpleValidation(t *testing.T) {
	tests := []struct {
		value, prefix string
		want          []string
		bad           bool
	}{
		{"", "p", []string{"p"}, false},
		{"literal", "p", []string{"pliteral"}, false},
		{"{,b}x{c,}", "p", []string{"pxc", "px", "pbxc", "pbx"}, false},
		{"a{b,c}d{e,f}", "", []string{"abde", "abdf", "acde", "acdf"}, false},
		{"{}", "", nil, true},
		{"{", "", nil, true},
		{"}", "", nil, true},
		{"{a,b", "", nil, true},
		{"a{b,{c,d}}", "", nil, true},
		{"a{b,c}x}", "", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			var got []string
			err := GlobExpandSimple(tt.value, tt.prefix, &got)
			if tt.bad {
				withCode, ok := err.(errs.ErrorWithCode)
				if !ok || withCode.Code != http.StatusBadRequest {
					t.Fatalf("malformed glob %q: want HTTP 400, got %v", tt.value, err)
				}
			} else if err != nil {
				t.Fatalf("valid glob %q: %v", tt.value, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("expand %q: got %q, want %q", tt.value, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("expand %q: got %q, want %q", tt.value, got, tt.want)
				}
			}
		})
	}
}

func TestGlobExpandSimpleDoesNotDiscardExistingResults(t *testing.T) {
	result := []string{"existing"}
	if err := GlobExpandSimple("{a,b}", "pre", &result); err != nil {
		t.Fatal(err)
	}
	want := []string{"existing", "prea", "preb"}
	if len(result) != len(want) {
		t.Fatalf("got %q, want %q", result, want)
	}
	for i := range want {
		if result[i] != want[i] {
			t.Fatalf("got %q, want %q", result, want)
		}
	}
}

func TestNonRegexpPrefixAlternation(t *testing.T) {
	tests := map[string]string{
		"foo.bar|baz":       "",
		"host=foo.bar|baz":  "host=",
		"host=~foo.bar|baz": "host=~",
		"host=foo=bar|baz":  "host=",
		"host=foo.bar":      "host=foo",
		"host=foo":          "host=foo",
		"":                  "",
	}
	for expr, want := range tests {
		if got := NonRegexpPrefix(expr); got != want {
			t.Errorf("NonRegexpPrefix(%q) = %q; want %q", expr, got, want)
		}
	}
	if got := ConcatMatchKV("host", ""); got != `host=\\%` {
		t.Errorf("ConcatMatchKV(host, empty) = %q", got)
	}
}

func TestQuoteAndINAreSQLLiterals(t *testing.T) {
	for _, value := range []string{"", `a' OR 1=1 --`, `a\b'`, "a%_"} {
		if !predicateMatches(t, Eq("metric", value), value) {
			t.Fatalf("equality must match literal %q", value)
		}
		if predicateMatches(t, Eq("metric", value), value+"suffix") {
			t.Fatalf("equality must reject extra suffix on %q", value)
		}
		if !predicateMatches(t, In("metric", []string{value}), value) {
			t.Fatalf("single-element IN must match %q", value)
		}
		if !predicateMatches(t, In("metric", []string{value, "other"}), value) {
			t.Fatalf("two-element IN must match %q", value)
		}
	}
}

func TestGlobMalformedUTF8(t *testing.T) {
	for _, query := range []string{"a*\xff", "\xfe?", "\xc3"} {
		for _, got := range []string{Glob("metric", query), TreeGlob("metric", query)} {
			if got != "0" {
				t.Errorf("malformed UTF-8 glob %q produced SQL %q, want false predicate", query, got)
			}
		}
	}
}

func TestMatchMalformedUTF8(t *testing.T) {
	for _, tc := range []struct{ key, value string }{{"key", "^\xff$"}, {"\xfe", "foo"}, {"\xfe", ""}, {"\xfe", "*"}} {
		if got := Match("metric", tc.key, tc.value); got != "0" {
			t.Errorf("malformed UTF-8 Match(%q, %q) = %q, want false predicate", tc.key, tc.value, got)
		}
	}
}

func FuzzGlobGeneratedSQL(f *testing.F) {
	for _, seed := range []string{"", "*", "foo*", "foo[", "foo[.]", "a{b,c}", "foo\\", "a%'_\\'", "[a-z]", "foo(bar)|baz"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, query string) {
		if len(query) > 64 {
			t.Skip()
		}
		for _, sql := range []string{Glob("metric", query), TreeGlob("metric", query)} {
			if !utf8.ValidString(query) {
				if sql != "0" {
					t.Fatalf("malformed UTF-8 glob %q: got %q", query, sql)
				}
				continue
			}
			predicateMatches(t, sql, query)
			predicateMatches(t, sql, "unrelated")
		}
	})
}

func FuzzMatchGeneratedSQL(f *testing.F) {
	for _, seed := range []struct{ key, value string }{{"host", ""}, {"host", "^foo$"}, {"a' OR 1=1 --", "^foo"}, {"a%_\\", "foo|bar"}, {"", "*"}} {
		f.Add(seed.key, seed.value)
	}
	f.Fuzz(func(t *testing.T, key, pattern string) {
		if len(key)+len(pattern) > 64 {
			t.Skip()
		}
		sql := Match("metric", key, pattern)
		if !utf8.ValidString(key) || !utf8.ValidString(pattern) {
			if sql != "0" {
				t.Fatalf("malformed UTF-8 Match(%q, %q): got %q", key, pattern, sql)
			}
			return
		}
		predicateMatches(t, sql, key+"=test")
		predicateMatches(t, sql, "unrelated")
	})
}

// Every regex metacharacter in a key is literal. Verify round-tripping both
// the SQL string literal and the compiled regex on arbitrary valid UTF-8.
func FuzzMatchKeyRoundTrip(f *testing.F) {
	for _, key := range []string{"", "host", `a.b|c[0]`, `a%'_\' OR 1=1 --`, "([{}])\\^$"} {
		f.Add(key)
	}
	f.Fuzz(func(t *testing.T, key string) {
		if len(key) > 64 || !utf8.ValidString(key) {
			t.Skip()
		}
		for _, pattern := range []string{"^literal$", "^l.*$", "literal"} {
			sql := Match("metric", key, pattern)
			if !predicateMatches(t, sql, key+"=literal") {
				t.Fatalf("key %q, pattern %q did not match itself; SQL %q", key, pattern, sql)
			}
			if predicateMatches(t, sql, "wrong"+key+"=literal") {
				t.Fatalf("key %q, pattern %q matched wrong key; SQL %q", key, pattern, sql)
			}
		}
	})
}

func FuzzPrefixRoundTrip(f *testing.F) {
	for _, prefix := range []string{"", "foo", `a%'_\' OR 1=1 --`, "(.*)[]{}$^", "О.б"} {
		f.Add(prefix)
	}
	f.Fuzz(func(t *testing.T, prefix string) {
		if len(prefix) > 64 || !utf8.ValidString(prefix) {
			t.Skip()
		}
		if !predicateMatches(t, HasPrefix("metric", prefix), prefix+"suffix") {
			t.Fatalf("LIKE prefix %q failed to match itself", prefix)
		}
		if !predicateMatches(t, HasPrefixBytes([]byte("metric"), []byte(prefix)), prefix) {
			t.Fatalf("LIKE prefix bytes %q failed to match itself", prefix)
		}
		if prefix != "" {
			first, size := utf8.DecodeRuneInString(prefix)
			other := 'x'
			if first == other {
				other = 'y'
			}
			if predicateMatches(t, HasPrefix("metric", prefix), string(other)+prefix[size:]) {
				t.Fatalf("LIKE prefix %q matched unrelated value", prefix)
			}
		}
	})
}

func FuzzGlobLiteralRegexRoundTrip(f *testing.F) {
	for _, literal := range []string{"", "foo", "a+b|c^$", `a%'_\'`, "О.б", "(foo)[bar]"} {
		f.Add(literal)
	}
	f.Fuzz(func(t *testing.T, literal string) {
		if len(literal) > 64 || !utf8.ValidString(literal) {
			t.Skip()
		}
		// A literal prefix before a wildcard exercises the generated regex
		// and the LIKE prefilter simultaneously.
		query := literal + "*"
		if strings.ContainsAny(literal, `*?{}[]\`) {
			return // those characters are glob operators, not literals
		}
		sql := Glob("metric", query)
		if !predicateMatches(t, sql, literal) || !predicateMatches(t, sql, literal+"x") {
			t.Fatalf("glob %q failed to match literal prefix; SQL %q", query, sql)
		}
		if literal != "" && predicateMatches(t, sql, "!"+literal) && !strings.HasPrefix("!"+literal, literal) {
			t.Fatalf("glob %q matched wrong prefix; SQL %q", query, sql)
		}
	})
}
