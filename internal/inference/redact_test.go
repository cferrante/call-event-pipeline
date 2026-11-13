package inference

import "testing"

func TestRedact(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Call me on +14158675310 please", "Call me on [phone] please"},
		{"my cell is (415) 867-5310.", "my cell is [phone]."},
		{"reach me at 415.867.5310 or 415 867 5310", "reach me at [phone] or [phone]"},
		{"email jordan.lee+ads@example.co.uk today", "email [email] today"},
		{"Mail ME@Example.COM, or call 4158675310", "Mail [email], or call [phone]"},
		// Short numbers aren't phone numbers: quantities, prices and model numbers stay.
		{"Need 2 units of model 3000 for $1,250", "Need 2 units of model 3000 for $1,250"},
		{"no personal data here", "no personal data here"},
	}
	for _, tt := range tests {
		if got := Redact(tt.in); got != tt.want {
			t.Errorf("Redact(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
