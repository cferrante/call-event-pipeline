package inference

import "regexp"

var (
	emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// A run of digits and the separators phone numbers are written with; it counts as a phone
	// number only with at least 7 digits, so quantities and prices are left alone.
	phonePattern = regexp.MustCompile(`\+?\(?\d[\d\s().-]{5,}\d`)
)

// Redact replaces email addresses and phone numbers in s with [email] and [phone], so personal
// data never reaches a hosted model. It errs towards removing too much: a long number that
// isn't a phone number (an order number, say) is removed too.
func Redact(s string) string {
	s = emailPattern.ReplaceAllString(s, "[email]")
	return phonePattern.ReplaceAllStringFunc(s, func(m string) string {
		digits := 0
		for _, r := range m {
			if r >= '0' && r <= '9' {
				digits++
			}
		}
		if digits >= 7 {
			return "[phone]"
		}
		return m
	})
}
