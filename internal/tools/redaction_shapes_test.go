package tools

import (
	"strings"
	"testing"
)

// A credential is masked in the shapes code and configuration give it: a
// typed declaration, an indexed assignment, a call's literal argument, a
// hash, an element, a name beside a value field, a name with a suffix such as
// _PASS, a signed URL and a webhook. Each is checked in the result and in the
// stream.
func TestCredentialShapesInCodeAndConfig(t *testing.T) {
	for _, tc := range []struct{ name, in, secret string }{
		{"rust const", `const API_TOKEN: &str = "r3rust1-7d102e";`, "r3rust1-7d102e"},
		{"rust static lifetime", `static API_TOKEN: &'static str = "r3rust2-7d102e";`, "r3rust2-7d102e"},
		{"rust String", `let password: String = "r3rust3-7d102e".to_string();`, "r3rust3-7d102e"},
		{"rust Option", `let api_key: Option<&str> = Some("r3rust4-7d102e");`, "r3rust4-7d102e"},
		{"go var typed", `var apiToken string = "r3go1-7d102e"`, "r3go1-7d102e"},
		{"go var password", `var password string = "r3go2-7d102e"`, "r3go2-7d102e"},
		{"go const token", `const apiToken string = "r3go3-7d102e"`, "r3go3-7d102e"},
		{"kotlin typed", `val apiKey: String = "r3kt1-7d102e"`, "r3kt1-7d102e"},
		{"kotlin nullable", `val apiKey: String? = "r3kt2-7d102e"`, "r3kt2-7d102e"},
		{"typescript typed", `const token: string = "r3ts1-7d102e";`, "r3ts1-7d102e"},
		{"python typed", `password: str = "r3py1-7d102e"`, "r3py1-7d102e"},
		{"python Optional", `password: Optional[str] = 'r3py2-7d102e'`, "r3py2-7d102e"},
		{"java field", `private static final String PASSWORD = "r3java1-7d102e";`, "r3java1-7d102e"},
		{"c++ std::string", `const std::string apiKey = "r3cpp1-7d102e";`, "r3cpp1-7d102e"},
		{"python index", `config["password"] = "r3idx1-7d102e"`, "r3idx1-7d102e"},
		{"python single-quoted index", `os.environ['API_TOKEN'] = 'r3idx2-7d102e'`, "r3idx2-7d102e"},
		{"js index", `settings["apiKey"] = "r3idx3-7d102e";`, "r3idx3-7d102e"},
		{"go os.Setenv", `os.Setenv("API_TOKEN", "r3call1-7d102e")`, "r3call1-7d102e"},
		{"go os.Setenv no space", `os.Setenv("API_TOKEN","r3call2-7d102e")`, "r3call2-7d102e"},
		{"viper.Set", `viper.Set("api_key", "r3call3-7d102e")`, "r3call3-7d102e"},
		{"php define", `define('DB_PASSWORD', 'r3php1-7d102e');`, "r3php1-7d102e"},
		{"php array", `'password' => 'r3php2-7d102e',`, "r3php2-7d102e"},
		{"php array double", `"secret" => "r3php3-7d102e",`, "r3php3-7d102e"},
		{"ruby hash", `password: "r3rb1-7d102e",`, "r3rb1-7d102e"},
		{"xml element", `<password>r3xml1-7d102e</password>`, "r3xml1-7d102e"},
		{"xml element spaced", "<apiKey> r3xml2-7d102e </apiKey>", "r3xml2-7d102e"},
		{"xml add value", `<add key="DB_PASSWORD" value="r3xml3-7d102e"/>`, "r3xml3-7d102e"},
		{"k8s env pair", "env:\n- name: DB_PASSWORD\n  value: r3k8s1-7d102e\n", "r3k8s1-7d102e"},
		{"k8s env pair quoted", "env:\n- name: DB_PASSWORD\n  value: \"r3k8s2-7d102e\"\n", "r3k8s2-7d102e"},
		{"k8s env pair json", `{"name":"API_TOKEN","value":"r3k8s3-7d102e"}`, "r3k8s3-7d102e"},
		{"ecs environment", `"environment":[{"name":"DB_PASSWORD","value":"r3ecs1-7d102e"}]`, "r3ecs1-7d102e"},
		{"suffix _PASS", "SMTP_PASS=r3pass1-7d102e", "r3pass1-7d102e"},
		{"suffix _PASS yaml", "MAIL_PASS: r3pass2-7d102e", "r3pass2-7d102e"},
		{"flag --db-pass", "run --db-pass r3pass3-7d102e", "r3pass3-7d102e"},
		{"app key", "APP_KEY=base64:r3app1-7d102eXk92mQzpR7", "r3app1-7d102eXk92mQzpR7"},
		{"master key", "MASTER_KEY=r3master1-7d102e", "r3master1-7d102e"},
		{"masterKey", `masterKey: "r3master2-7d102e"`, "r3master2-7d102e"},
		{"sas signature", "https://acct.blob.test/c/b?sv=2020&sig=r3sig1%2B7d102eAbCdEf&se=2030", "r3sig1%2B7d102eAbCdEf"},
		{"aws presigned", "https://b.s3.test/k?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=r3sig27d102e0123456789abcdef", "r3sig27d102e0123456789abcdef"},
		{"signature param", "GET /cb?Signature=r3sig3-7d102e&Expires=1", "r3sig3-7d102e"},
		{"x-goog-signature", "x-goog-signature=r3sig4-7d102e0123", "r3sig4-7d102e0123"},
		{"slack webhook", "curl -X POST https://hooks.slack.com/services/T0R3TEST/B0R3TEST/r3SlackSecret7d102eAbc -d x", "r3SlackSecret7d102eAbc"},
		{"discord webhook", "https://discord.com/api/webhooks/123456789012345678/r3DiscordSecret-7d102e_AbCdEf", "r3DiscordSecret-7d102e_AbCdEf"},
		{"discordapp webhook", "https://discordapp.com/api/webhooks/123456789012345678/r3DiscordSecret2-7d102e", "r3DiscordSecret2-7d102e"},
		{"teams webhook", "https://contoso.webhook.office.com/webhookb2/aaaa-bbbb@cccc-dddd/IncomingWebhook/r3teams7d102e0123/eeee-ffff", "r3teams7d102e0123"},
		{"outlook webhook", "https://outlook.office.com/webhook/aaaa-bbbb@cccc-dddd/IncomingWebhook/r3teams27d102e0123/eeee-ffff", "r3teams27d102e0123"},
	} {
		assertChainHides(t, tc.name, tc.in, tc.secret)
	}
}

// PWD is the working directory, and a name that merely contains a credential
// word's letters is not a credential.
func TestCredentialShapesKeepOrdinaryNames(t *testing.T) {
	for _, in := range []string{
		"PWD=/home/user/project",
		"export PWD=/tmp/x",
		"cd $PWD && ls",
		"OLDPWD=/home/user",
		"https://hooks.slack.com/",
		"https://discord.com/api/webhooks",
		"https://example.test/path?sort=asc&sig_type=1",
		"the signature of the function is unchanged",
		"const signature = computeSignature(payload)",
		"https://acct.blob.test/c/b?sv=2020&se=2030",
	} {
		assertChainShows(t, in, in)
	}
	assertChainKeeps(t, "masked name stays", "SMTP_PASS=r3pass9-7d102e", "SMTP_PASS=")
	if got := RedactSensitive("APP_KEY=base64:r3app9-7d102eXk92mQzpR7"); !strings.HasPrefix(got, "APP_KEY=") {
		t.Errorf("the name of a masked value was lost: %s", got)
	}
}

// A container nested deeper than the tracker follows keeps the mode of the
// deepest one it followed: what a credential's name opened stays masked, at
// any depth, in a document, in a log line and across the lines of a stream.
func TestContainersDeeperThanTheTrackerStayMasked(t *testing.T) {
	for _, depth := range []int{64, 65, 66, 100, 1000} {
		open, closing := strings.Repeat(`[`, depth), strings.Repeat(`]`, depth)
		for name, document := range map[string]string{
			"singular":      `{"api_key":` + open + `"r3deep1-7d102e"` + closing + `}`,
			"plural":        `{"apiKeys":` + open + `"r3deep2-7d102e"` + closing + `}`,
			"objects":       `{"password":` + strings.Repeat(`{"a":`, depth) + `"r3deep3-7d102e"` + strings.Repeat(`}`, depth) + `}`,
			"in a log line": "loaded " + `{"credentials":` + open + `"r3deep4-7d102e"` + closing + `}` + " ok",
			"number":        `{"secret":` + open + `4242424` + closing + `}`,
		} {
			for _, hidden := range []string{"r3deep1-7d102e", "r3deep2-7d102e", "r3deep3-7d102e", "r3deep4-7d102e", "4242424"} {
				if got := RedactSensitive(document); strings.Contains(got, hidden) {
					t.Errorf("%s at depth %d leaked %q in the result", name, depth, hidden)
				}
				if got := streamRedact(document); strings.Contains(got, hidden) {
					t.Errorf("%s at depth %d leaked %q in the stream", name, depth, hidden)
				}
			}
		}
		// Pretty-printed, one level per line.
		var lines []string
		lines = append(lines, `"api_key": [`)
		for i := 0; i < depth; i++ {
			lines = append(lines, strings.Repeat(" ", i+1)+`[`)
		}
		lines = append(lines, `"r3deep5-7d102e"`)
		if got := streamRedact(strings.Join(lines, "\n")); strings.Contains(got, "r3deep5-7d102e") {
			t.Errorf("a pretty-printed container at depth %d leaked in the stream", depth)
		}
	}
	// Depth alone is not a credential: an ordinary document stays as it is.
	plain := `{"data":` + strings.Repeat(`[`, 100) + `"r3visible"` + strings.Repeat(`]`, 100) + `}`
	assertChainShows(t, "an ordinary deep document", plain)
}
