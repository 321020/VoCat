package device

import (
	"testing"

	"vocat/internal/modem"
)

func TestParseRegistrationStatus(t *testing.T) {
	tests := []struct {
		line string
		want int
	}{
		{line: "+CEREG: 0,5", want: 5},
		{line: "+CGREG: 2,1,\"FFFE\",\"06698D06\",7", want: 1},
		{line: "+CREG: 2", want: 2},
	}
	for _, test := range tests {
		got, ok := parseRegistrationStatus(modem.Response{Lines: []string{test.line}})
		if !ok || got != test.want {
			t.Fatalf("parseRegistrationStatus(%q) = %d, %v", test.line, got, ok)
		}
	}
}
