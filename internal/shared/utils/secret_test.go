package utils

import "testing"

func TestConstantTimeEqualString(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{
			name: "equal strings",
			a:    "server-secret",
			b:    "server-secret",
			want: true,
		},
		{
			name: "different same length strings",
			a:    "server-secret",
			b:    "wrong--secret",
			want: false,
		},
		{
			name: "different length strings",
			a:    "short",
			b:    "much-longer-secret",
			want: false,
		},
		{
			name: "empty strings are equal",
			a:    "",
			b:    "",
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ConstantTimeEqualString(tt.a, tt.b); got != tt.want {
				t.Fatalf("ConstantTimeEqualString() = %v, want %v", got, tt.want)
			}
		})
	}
}
