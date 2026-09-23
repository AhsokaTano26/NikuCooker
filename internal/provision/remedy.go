package provision

import "strings"

// The failures a user is actually likely to hit, and what to do about them.
//
// This is the highest-value small thing in the package. When provisioning fails
// it fails inside a download, and what reaches the screen is uv's own text —
// precise, and written for someone who already knows what a certificate chain
// is. Printing a line of advice above it is the difference between a user who
// fixes it and a user who gives up.
//
// Matching is on lowercased substrings, deliberately loose: uv's exact wording
// is not a contract, and a remedy that appears for a neighbouring error is
// cheaper than one that never appears at all.
var remedies = []struct {
	needles []string
	advice  string
}{
	{
		// uv uses rustls, not the operating system's trust store, so a proxy
		// that re-signs TLS produces this and the machine's browser is perfectly
		// happy — which makes it look like the program is broken.
		needles: []string{"invalid peer certificate", "unknownissuer", "certificate verify failed"},
		advice: "Your network appears to be re-signing TLS, which uv does not trust by default. " +
			"Point SSL_CERT_FILE at your organisation's CA bundle, or set UV_NATIVE_TLS=1 to use the system trust store.",
	},
	{
		needles: []string{"proxy", "tunnel connection failed", "407"},
		advice: "A proxy is in the way. Set HTTPS_PROXY (and HTTP_PROXY) to your proxy's address " +
			"and start the server again.",
	},
	{
		needles: []string{"no space left", "enospc"},
		advice: "The disk filled up. Free some space, then install again — " +
			"a partial environment is removed automatically before the next attempt.",
	},
	{
		needles: []string{"permission denied", "access is denied", "eacces"},
		advice: "Something in the data directory is not writable. Check its permissions, " +
			"or point --data-dir somewhere the current user can write.",
	},
	{
		needles: []string{"failed to fetch", "connection refused", "dns error", "timed out", "network is unreachable"},
		advice: "The download could not reach the network. Check the connection, then install again — " +
			"what was already downloaded is kept.",
	},
	{
		needles: []string{"the system cannot find the file", "no such file or directory"},
		advice: "A file the install needs is missing. If the program was moved after it was extracted, " +
			"extract it again — the environment records absolute paths.",
	},
}

// remedy returns advice for an error message, or "" when nothing matches.
func remedy(message string) string {
	lower := strings.ToLower(message)
	for _, entry := range remedies {
		for _, needle := range entry.needles {
			if strings.Contains(lower, needle) {
				return entry.advice
			}
		}
	}
	return ""
}

// errorCode classifies a failure into a stable code.
//
// The codes are coarse on purpose. The message and the remedy are what a person
// reads; this exists so that a client can branch without matching on prose, and
// there are only as many of them as there are distinguishable situations.
func errorCode(message string) string {
	lower := strings.ToLower(message)

	switch {
	case strings.Contains(lower, "not enough free space"):
		return "PROVISION_NO_SPACE"
	case strings.Contains(lower, "could not be run"):
		return "PROVISION_UV_UNUSABLE"
	case strings.Contains(lower, "source is incomplete"):
		return "PROVISION_SOURCE_MISSING"
	case strings.Contains(lower, "cannot write to the data directory"):
		return "PROVISION_NOT_WRITABLE"
	case strings.Contains(lower, "invalid peer certificate"),
		strings.Contains(lower, "unknownissuer"),
		strings.Contains(lower, "certificate verify failed"):
		return "PROVISION_TLS"
	case strings.Contains(lower, "proxy"),
		strings.Contains(lower, "tunnel connection failed"):
		return "PROVISION_PROXY"
	case strings.Contains(lower, "no space left"):
		return "PROVISION_NO_SPACE"
	case strings.Contains(lower, "failed to fetch"),
		strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "timed out"):
		return "PROVISION_NETWORK"
	case strings.Contains(lower, "does not work"):
		return "PROVISION_SELFCHECK_FAILED"
	default:
		return "PROVISION_FAILED"
	}
}
