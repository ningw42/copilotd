package bareversion

import "testing"

func TestValid(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "1.2.3", want: true},
		{value: "1.2.3-insider.1", want: true},
		{value: "1.2.3+build.7", want: true},
		{value: "1.2.3-rc.1+build.7", want: true},
		{value: "latest"},
		{value: "v1.2.3"},
		{value: "1.2"},
		{value: "1.2.3-"},
		{value: ""},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			if got := Valid(test.value); got != test.want {
				t.Errorf("Valid(%q) = %t, want %t", test.value, got, test.want)
			}
		})
	}
}
