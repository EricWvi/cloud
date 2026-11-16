package agentruntime

import (
	"encoding/base64"
	"encoding/hex"
)

// secretForms returns each secret as written and in the encodings a payload could carry it in
// without spelling it out: hex, and standard and URL-safe base64 at each of the three byte
// alignments the secret can start at (as in a Basic credential or a JSON []byte). For base64 it
// keeps only the characters every bit of which comes from the secret, so the form matches
// whatever bytes surround the secret.
func secretForms(secrets ...string) []string {
	var forms []string
	seen := map[string]bool{}
	add := func(form string) {
		if !seen[form] {
			seen[form] = true
			forms = append(forms, form)
		}
	}
	for _, secret := range secrets {
		add(secret)
		add(hex.EncodeToString([]byte(secret)))
		for pad := 0; pad < 3; pad++ {
			first := (8*pad + 5) / 6
			last := 8 * (pad + len(secret)) / 6
			for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.URLEncoding} {
				encoded := encoding.EncodeToString(append(make([]byte, pad), secret...))
				add(encoded[first:last])
			}
		}
	}
	return forms
}
