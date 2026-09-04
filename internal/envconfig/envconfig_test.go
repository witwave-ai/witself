package envconfig

import "testing"

func TestEnvironmentFallbackPolicies(t *testing.T) {
	lookup := func(values map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) {
			value, ok := values[key]
			return value, ok
		}
	}

	tests := []struct {
		name string
		call func(func(string) (string, bool), string, string) string
		want map[string]string
	}{
		{
			name: "raw OS value",
			call: RawOr,
			want: map[string]string{
				"missing": "fallback", "empty": "fallback",
				"blank": " \t", "padded": " value ",
			},
		},
		{
			name: "trimmed nonblank value",
			call: TrimmedOr,
			want: map[string]string{
				"missing": "fallback", "empty": "fallback",
				"blank": "fallback", "padded": "value",
			},
		},
		{
			name: "trimmed present value",
			call: TrimmedPresentOr,
			want: map[string]string{
				"missing": "fallback", "empty": "",
				"blank": "", "padded": "value",
			},
		},
	}
	values := map[string]string{"empty": "", "blank": " \t", "padded": " value "}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for key, want := range test.want {
				if got := test.call(lookup(values), key, "fallback"); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
		})
	}
}
