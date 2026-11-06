package server

import "testing"

func TestSQLPrefixSkipsLeadingComments(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`/* {"app": "dbt"} */ select 1`, "select 1"},
		{`/* a */ /* b */ select 2`, "select 2"},
		{`select 3 /* trailing */`, "select 3 /* trailin"},
		{`/* unterminated select 4`, "/* unterminated sel"},
	} {
		if got := sqlPrefix(tc.in, 19); got != tc.want {
			t.Errorf("sqlPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
