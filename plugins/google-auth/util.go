package main

import "strings"

func localPart(email string) string {
	if i := strings.Index(email, "@"); i > 0 {
		return email[:i]
	}
	return email
}

// httpsOnly returns raw if it is an absolute https URL, else "". The host
// ignores pictures that are not absolute http(s) URLs; Google serves https.
func httpsOnly(raw string) string {
	if strings.HasPrefix(raw, "https://") && len(raw) > len("https://") {
		return raw
	}
	return ""
}
