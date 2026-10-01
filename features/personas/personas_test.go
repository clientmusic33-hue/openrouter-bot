package personas

import "testing"

func TestPersonaLookupAndAliases(t *testing.T) {
	list := List()
	if len(list) < 8 {
		t.Fatalf("expected at least 8 built-in personas, got %d", len(list))
	}

	cases := map[string]string{
		"developer":          "developer",
		"Developer":          "developer",
		"dev":                "developer",
		"coding agent":       "coding_agent",
		"business assistant": "business",
		"teacher":            "teacher",
		"researcher":         "researcher",
		"writer":             "writer",
		"translator":         "translator",
	}

	for input, wantKey := range cases {
		p, ok := Lookup(input)
		if !ok {
			t.Fatalf("Lookup(%q) failed", input)
		}
		if p.Key != wantKey {
			t.Errorf("Lookup(%q).Key = %q, want %q", input, p.Key, wantKey)
		}
	}
}
