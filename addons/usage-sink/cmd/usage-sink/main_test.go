package main

import "testing"

func TestResolveMode(t *testing.T) {
	cases := []struct {
		name          string
		explicit      string
		duckdbPathSet bool
		mdDatabase    string
		want          string
	}{
		{"default stdout", "", false, "", "stdout"},
		{"explicit both overrides defaults", "both", true, "prod", "both"},
		{"explicit duckdb-path flag or env enables duckdb", "", true, "", "duckdb"},
		{"motherduck database enables duckdb", "", false, "analytics", "duckdb"},
		{"explicit stdout overrides duckdb hints", "stdout", true, "analytics", "stdout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveMode(tc.explicit, tc.duckdbPathSet, tc.mdDatabase); got != tc.want {
				t.Fatalf("resolveMode(%q, %v, %q) = %q, want %q", tc.explicit, tc.duckdbPathSet, tc.mdDatabase, got, tc.want)
			}
		})
	}
}
