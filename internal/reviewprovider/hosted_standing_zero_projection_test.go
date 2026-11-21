package reviewprovider

import "testing"

func TestStandingInventoryKeepsVisibleCountRefusalSeparateFromStructure(t *testing.T) {
	cases := []struct {
		name  string
		lines []findingLine
		want  bool
	}{
		{"unavailable projection", []findingLine{{hidden: true}}, false},
		{"visible ordinary prose", []findingLine{{hidden: true, countText: "Summary is available."}}, false},
		{"visible current counter", []findingLine{{hidden: true, countText: "Current findings: 2"}}, true},
		{"visible counter continuation", []findingLine{{hidden: true, countContinues: true, countText: "Current "}, {hidden: true, countText: "findings: 2"}}, true},
		{"visible actionable counter", []findingLine{{hidden: true, countText: "Actionable comments posted: 2"}}, true},
		{"structural marker", []findingLine{{text: "cr-comment"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := standingInventoryContradictsZero(tc.lines, &standingHistoricalParser{}); got != tc.want {
				t.Fatalf("contradiction = %t, want %t", got, tc.want)
			}
		})
	}
}
