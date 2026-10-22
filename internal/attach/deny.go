// SPDX-License-Identifier: GPL-3.0-or-later

package attach

import (
	"path/filepath"
	"strings"
)

// credentialPaths are the places under the home directory where tools
// keep logins, keys and tokens, on any OS (a path that does not exist
// costs nothing). Agents never attach anything from them, whatever their
// working directory: the fleet tools read as the user, outside the
// sandbox, so these are exactly what a sandbox profile denies.
var credentialPaths = []string{
	// Keys and generic token files.
	".ssh", ".gnupg", ".password-store", ".pki", ".netrc", "_netrc", ".git-credentials",
	".config/git/credentials", ".vault-token", ".pgpass", ".my.cnf", ".local/share/keyrings",
	".local/share/kwalletd", ".config/op", ".1password",
	// Agents' logins and transcripts.
	".codex", ".claude", ".claude.json", ".gemini", ".antigravity", ".config/goose",
	".local/share/goose", ".config/opencode", ".local/share/opencode", ".config/github-copilot",
	".cursor", ".aider", ".config/Claude",
	// Forges, packages and registries.
	".config/gh", ".config/hub", ".config/glab-cli", ".config/copr", ".npmrc", ".yarnrc",
	".yarnrc.yml", ".pypirc", ".cargo/credentials", ".cargo/credentials.toml", ".gem/credentials",
	".m2/settings.xml", ".gradle/gradle.properties", ".nuget/NuGet/NuGet.Config", ".docker",
	".config/containers/auth.json",
	// Clouds and clusters.
	".aws", ".azure", ".config/gcloud", ".kube", ".oci", ".config/doctl", ".s3cfg", ".boto",
	".terraform.d", ".config/rclone", ".config/hcloud", ".config/flyctl", ".fly",
	// Browsers and mail (signed-in cookies).
	".mozilla", ".thunderbird", ".config/google-chrome", ".config/chromium",
	".config/BraveSoftware", ".config/microsoft-edge", ".config/vivaldi", ".config/opera",
	".var/app",
	// macOS.
	"Library/Keychains", "Library/Cookies", "Library/Safari", "Library/Mail", "Library/Messages",
	"Library/Containers/com.apple.Safari", "Library/Application Support/Google",
	"Library/Application Support/Firefox", "Library/Application Support/BraveSoftware",
	"Library/Application Support/Microsoft Edge", "Library/Application Support/Claude",
	"Library/Application Support/Antigravity", "Library/Application Support/Code",
	"Library/Application Support/com.apple.TCC", "Library/Group Containers",
	// Windows.
	"AppData/Roaming/Microsoft/Credentials", "AppData/Local/Microsoft/Credentials",
	"AppData/Roaming/Microsoft/Protect", "AppData/Roaming/Microsoft/Crypto",
	"AppData/Roaming/Microsoft/SystemCertificates", "AppData/Roaming/Mozilla",
	"AppData/Local/Google/Chrome/User Data", "AppData/Local/Microsoft/Edge/User Data",
	"AppData/Local/BraveSoftware", "AppData/Roaming/GitHub CLI", "AppData/Roaming/Block/goose",
	"AppData/Roaming/Claude", "AppData/Roaming/Code", "AppData/Roaming/gcloud",
	"AppData/Roaming/npm/etc/npmrc",
}

// CredentialPaths returns credentialPaths under home, or nil without one.
func CredentialPaths(home string) []string {
	if home == "" {
		return nil
	}
	out := make([]string, len(credentialPaths))
	for i, p := range credentialPaths {
		out[i] = filepath.Join(home, filepath.FromSlash(p))
	}
	return out
}

// ExpandHome replaces a leading "~/" (or a bare "~") in p with home.
func ExpandHome(p, home string) string {
	switch {
	case p == "~":
		return home
	case strings.HasPrefix(p, "~/"), strings.HasPrefix(p, `~\`):
		if home == "" {
			return ""
		}
		return filepath.Join(home, filepath.FromSlash(p[2:]))
	}
	return p
}
