package api

import "testing"

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"Calendar Planner":                   "Calendar Planner",
		"\"inbox helper\".":                  "Inbox Helper",
		"Name: Trip Planner\n":               "Trip Planner",
		"**Planner**":                        "Planner",
		"THINK Ask the user":                 "",
		"New agent":                          "",
		"Calendar & Day Planner":             "",
		"Supercalifragilistic Planner Extra": "",
		"":                                   "",
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}
