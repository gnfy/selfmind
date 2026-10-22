package tools

import "testing"

// Lines of real code and prose that an earlier reading of quotes and weak
// names masked, kept as they are: a call that reads with an empty default, a
// docstring after a name, a type annotation, a comparison with punctuation, a
// template string, optional chaining, an encoding, a list of names, prose.
func TestRealCodeFalsePositivesStayUnmasked(t *testing.T) {
	for _, in := range []string{
		"agent = make(\n    current_api_key=str(getattr(state.agent, \"api_key\", \"\") or \"\"),\n)",
		`singleton_key = str((_read_codex_tokens().get("tokens") or {}).get("access_token", "") or "").strip()`,
		"def set_session_cwd(cwd: str | None) -> Token:\n    \"\"\"Pin the logical cwd for the current context.\"\"\"\n    return x",
		"class Token:\n    \"\"\"The token.\"\"\"",
		"    refresh_token: Optional[str] = None",
		`if token != "--" and not token.startswith("-"):`,
		"headers.set('authorization', `Bearer ${bearer}`)",
		"  Authorization: `Bearer ${process.env.GITHUB_TOKEN}`,",
		"const token = match?.[0]",
		"const tokens = {\n  refreshToken: String(body?.refresh_token || ''),\n}",
		"fs.writeFileSync(filePath, 'secret', 'utf8')",
		`_ALIAS_SUFFIXES = ("_API_KEY", "_TOKEN", "_SECRET", "_KEY", "_PASSWORD")`,
		"# No credentials: try the fallback chain BEFORE failing (an exhausted pool)",
		"credential = key_provider if callable(key_provider) else fb_client.api_key",
		`CapabilityCredentialRead = "credential:read"`,
		"AZURE_TOKEN_CREDENTIALS: prod",
		"self._micro_compact_turns_since_pass = 0",
		"if (typeof command.password !== \"string\") return",
		"REST uses an `X-Hermes-Session-Token` header; WS uses `?token=`.\n *  - 'oauth': hosted gateways",
		`echo "APPLE_API_KEY=$RUNNER_TEMP/apple-api-key.p8" >> "$GITHUB_ENV"`,
		`out[token] = struct{}{}`,
		`let mut secret_key = [0u8; 32];`,
	} {
		for surface, out := range map[string]string{"result": RedactSensitive(in), "stream": streamRedact(in)} {
			if out != in {
				t.Errorf("the %s masked:\n  in:  %q\n  out: %q", surface, in, out)
			}
		}
	}
}
