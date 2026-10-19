package psql

import (
	"strings"
	"testing"
)

func FuzzParseRoutine(f *testing.F) {
	seeds := []string{
		"",
		"DECLARE VARIABLE x INTEGER;\nbegin\n  x = 1;\nend",
		"begin\nif (x = 1) then begin suspend; end else exit;\nend",
		"begin\nfor select a from t into :a do\nbegin\n  when any do x = 1;\nend\nend",
		"begin\nexecute procedure p(:a, 'b') returning_values :c;\nend",
		"begin\n/* a /* b */ x = \"NOW\";\nwhen sqlcode -803, gdscode lock_conflict do exit;\nend",
		"begin\nexecute statement 'x';\nupdate t set a = 1 where current of c;\nend",
		"begin ((( end",
	}
	for _, seed := range seeds {
		f.Add(seed, 3)
		f.Add(seed, 1)
	}
	f.Fuzz(func(t *testing.T, body string, dialect int) {
		r, problems := ParseRoutine(body, ParseOptions{Dialect: dialect})
		if r == nil {
			t.Fatal("nil routine")
		}
		for _, p := range problems {
			if p.Line < 1 || p.Column < 1 || strings.HasPrefix(p.Message, "internal parser error") {
				t.Fatalf("problem position %+v", p)
			}
		}
		for i, n := range r.Nodes {
			if n.ID != i || n.Line < 1 || n.Column < 1 || n.EndLine < n.Line {
				t.Fatalf("node %d: %+v", i, n)
			}
		}
		_ = r.HasSuspend()
		_ = r.MayWrite()
		_ = r.Callees()
	})
}
