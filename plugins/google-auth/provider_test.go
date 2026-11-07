package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
)

const testClient = "cid.apps.googleusercontent.com"

func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(claims)
	return "x." + base64.RawURLEncoding.EncodeToString(b) + ".y"
}

func entries(t *testing.T, m map[string]any) []*pluginv1.ConfigEntry {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return []*pluginv1.ConfigEntry{{Key: "google", Value: s}}
}

// fakeGoogle answers the token endpoint; claims is built per request so the
// test can echo the nonce from the authorize step.
type fakeGoogle struct {
	srv      *httptest.Server
	claims   func(nonce string) map[string]any
	nonce    string
	lastForm url.Values
	err      string
	refresh  string
}

func newFake(t *testing.T) *fakeGoogle {
	f := &fakeGoogle{refresh: "rt-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.lastForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		if f.err != "" {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": f.err})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id_token": jwt(t, f.claims(f.nonce)), "refresh_token": f.refresh})
	})
	mux.HandleFunc("/disco", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) })
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newTestProvider(t *testing.T, f *fakeGoogle, cfg map[string]any) *provider {
	p := newProvider(hclog.NewNullLogger())
	p.ep = endpoints{Auth: f.srv.URL + "/auth", Token: f.srv.URL + "/token", Discovery: f.srv.URL + "/disco"}
	p.now = func() time.Time { return time.Unix(1_000_000, 0) }
	p.configure(entries(t, cfg))
	return p
}

func baseCfg() map[string]any {
	return map[string]any{"client_id": testClient, "client_secret": "s3", "silo_api_key": "sa_key"}
}

func goodClaims(nonce string) map[string]any {
	return map[string]any{"iss": "https://accounts.google.com", "aud": testClient, "sub": "1234", "exp": 2_000_000,
		"nonce": nonce, "email": "pete@gmail.com", "email_verified": true, "name": "Pete", "picture": "https://x/p.png"}
}

func start(t *testing.T, p *provider) (*pluginv1.InitAuthorizeResponse, url.Values) {
	t.Helper()
	r, err := p.InitAuthorize(context.Background(), &pluginv1.InitAuthorizeRequest{RedirectUri: "https://silo.example.com/cb", State: "st", Prompt: "select_account"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(r.AuthorizeUrl)
	return r, u.Query()
}

// fakeSilo answers GET /api/v2/admin/users?identity=.
func fakeSilo(t *testing.T, status int, users map[string]bool) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sa_key" {
			w.WriteHeader(401)
			return
		}
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		id := strings.ToLower(r.URL.Query().Get("identity"))
		items := []map[string]any{}
		for e, enabled := range users {
			if e == id {
				items = append(items, map[string]any{"email": strings.ToUpper(e), "enabled": enabled})
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "page": map[string]any{"has_more": false}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

var defaultUsers = map[string]bool{"pete@gmail.com": true, "invitee@gmail.com": true, "disabled@gmail.com": false}

func setup(t *testing.T, cfg map[string]any, silo *httptest.Server) (*provider, *fakeGoogle) {
	f := newFake(t)
	p := newTestProvider(t, f, cfg)
	if silo != nil {
		p.siloBase = silo.URL
	}
	return p, f
}

func signIn(t *testing.T, p *provider, f *fakeGoogle, mut func(m map[string]any)) *pluginv1.AuthenticateResponse {
	t.Helper()
	init, q := start(t, p)
	f.nonce = q.Get("nonce")
	f.claims = func(n string) map[string]any {
		m := goodClaims(n)
		if mut != nil {
			mut(m)
		}
		return m
	}
	resp, err := p.ExchangeCode(context.Background(), &pluginv1.ExchangeCodeRequest{Code: "c", RedirectUri: "https://silo.example.com/cb", ProviderState: init.ProviderState})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuthorizeURL(t *testing.T) {
	p, _ := setup(t, baseCfg(), nil)
	_, q := start(t, p)
	for k, want := range map[string]string{"client_id": testClient, "response_type": "code", "scope": "openid email profile",
		"state": "st", "code_challenge_method": "S256", "prompt": "select_account", "redirect_uri": "https://silo.example.com/cb"} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if q.Get("code_challenge") == "" || q.Get("nonce") == "" {
		t.Error("missing PKCE challenge or nonce")
	}
}

func TestSignInSuccessAndNeverSetsRole(t *testing.T) {
	p, f := setup(t, baseCfg(), fakeSilo(t, 200, defaultUsers))
	init, q := start(t, p)
	f.nonce = q.Get("nonce")
	f.claims = goodClaims
	resp, err := p.ExchangeCode(context.Background(), &pluginv1.ExchangeCodeRequest{Code: "c", RedirectUri: "https://silo.example.com/cb", ProviderState: init.ProviderState})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ExternalSubject != "1234" || resp.Email != "pete@gmail.com" {
		t.Fatalf("bad response: %+v", resp)
	}
	if resp.ManagedRole != pluginv1.AuthManagedRole_AUTH_MANAGED_ROLE_UNSPECIFIED {
		t.Errorf("the plugin must leave the role to Silo, got %v", resp.ManagedRole)
	}
	if resp.RefreshState.AsMap()["refresh_token"] != "rt-1" {
		t.Error("refresh token not kept")
	}
	if f.lastForm.Get("code_verifier") == "" || pkceChallenge(f.lastForm.Get("code_verifier")) != q.Get("code_challenge") {
		t.Error("code_verifier does not match the challenge")
	}
	// Case-insensitive match, as an invited user types their address any way.
	if r := signIn(t, p, f, func(m map[string]any) { m["email"] = "Invitee@Gmail.com" }); r.ExternalSubject == "" {
		t.Errorf("invitee should sign in: %+v", r)
	}
}

func TestSignInRefusals(t *testing.T) {
	cases := map[string]struct {
		mut  func(m map[string]any)
		want pluginv1.AuthDenial
	}{
		"no silo account":  {func(m map[string]any) { m["email"] = "stranger@gmail.com" }, pluginv1.AuthDenial_AUTH_DENIAL_NOT_PERMITTED},
		"disabled account": {func(m map[string]any) { m["email"] = "disabled@gmail.com" }, pluginv1.AuthDenial_AUTH_DENIAL_NOT_PERMITTED},
		"unverified email": {func(m map[string]any) { m["email_verified"] = false }, pluginv1.AuthDenial_AUTH_DENIAL_NOT_PERMITTED},
		"wrong audience":   {func(m map[string]any) { m["aud"] = "other" }, pluginv1.AuthDenial_AUTH_DENIAL_INVALID_CREDENTIALS},
		"expired":          {func(m map[string]any) { m["exp"] = 5 }, pluginv1.AuthDenial_AUTH_DENIAL_INVALID_CREDENTIALS},
		"bad nonce":        {func(m map[string]any) { m["nonce"] = "replayed" }, pluginv1.AuthDenial_AUTH_DENIAL_INVALID_CREDENTIALS},
		"wrong issuer":     {func(m map[string]any) { m["iss"] = "https://evil.example" }, pluginv1.AuthDenial_AUTH_DENIAL_INVALID_CREDENTIALS},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, f := setup(t, baseCfg(), fakeSilo(t, 200, defaultUsers))
			resp := signIn(t, p, f, tc.mut)
			if resp.ExternalSubject != "" || resp.Denial != tc.want {
				t.Fatalf("got subject=%q denial=%v, want empty subject and %v", resp.ExternalSubject, resp.Denial, tc.want)
			}
		})
	}
}

func TestNoSiloKeyFailsClosed(t *testing.T) {
	cfg := baseCfg()
	delete(cfg, "silo_api_key")
	p, f := setup(t, cfg, fakeSilo(t, 200, defaultUsers))
	if r := signIn(t, p, f, nil); r.ExternalSubject != "" || r.Denial != pluginv1.AuthDenial_AUTH_DENIAL_PROVIDER_UNAVAILABLE {
		t.Fatalf("without a key nobody may sign in: %+v", r)
	}
}

func TestSiloLookupFailureFailsClosed(t *testing.T) {
	for _, st := range []int{500, 401} {
		p, f := setup(t, baseCfg(), fakeSilo(t, st, nil))
		if r := signIn(t, p, f, nil); r.ExternalSubject != "" || r.Denial != pluginv1.AuthDenial_AUTH_DENIAL_PROVIDER_UNAVAILABLE {
			t.Errorf("status %d: want PROVIDER_UNAVAILABLE denial, got %+v", st, r)
		}
	}
}

func TestInvalidGrantAndUnconfigured(t *testing.T) {
	p, f := setup(t, baseCfg(), fakeSilo(t, 200, defaultUsers))
	init, _ := start(t, p)
	f.err = "invalid_grant"
	resp, _ := p.ExchangeCode(context.Background(), &pluginv1.ExchangeCodeRequest{Code: "c", ProviderState: init.ProviderState})
	if resp.Denial != pluginv1.AuthDenial_AUTH_DENIAL_INVALID_CREDENTIALS || resp.ExternalSubject != "" {
		t.Fatalf("%+v", resp)
	}
	empty, _ := setup(t, map[string]any{}, nil)
	if _, err := empty.InitAuthorize(context.Background(), &pluginv1.InitAuthorizeRequest{RedirectUri: "r", State: "s"}); err == nil {
		t.Error("unconfigured InitAuthorize should error")
	}
	empty.configure(nil)
}

func TestCheckAccount(t *testing.T) {
	p, f := setup(t, baseCfg(), fakeSilo(t, 200, map[string]bool{"pete@gmail.com": true}))
	rs, _ := structpb.NewStruct(map[string]any{"refresh_token": "rt-1"})
	f.claims = func(string) map[string]any { return goodClaims("") }
	check := func(subject string, state *structpb.Struct) pluginv1.CheckAccountStatus {
		r, err := p.CheckAccount(context.Background(), &pluginv1.CheckAccountRequest{ExternalSubject: subject, RefreshState: state})
		if err != nil {
			t.Fatal(err)
		}
		return r.Status
	}
	if got := check("1234", rs); got != pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_ACTIVE {
		t.Fatalf("status %v", got)
	}
	if got := check("9999", rs); got == pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_ACTIVE {
		t.Error("subject mismatch must not be ACTIVE")
	}
	if got := check("1234", nil); got != pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_UNSUPPORTED {
		t.Errorf("no refresh token should be UNSUPPORTED, got %v", got)
	}
	f.claims = func(string) map[string]any { m := goodClaims(""); m["email_verified"] = false; return m }
	if got := check("1234", rs); got != pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_NOT_PERMITTED {
		t.Errorf("unverified email want NOT_PERMITTED, got %v", got)
	}
	f.claims = func(string) map[string]any { return goodClaims("") }
	f.err = "invalid_grant"
	if got := check("1234", rs); got != pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_UNSUPPORTED {
		t.Errorf("invalid_grant should be UNSUPPORTED, got %v", got)
	}
	f.err = "temporarily_unavailable"
	if got := check("1234", rs); got != pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_UNAVAILABLE {
		t.Errorf("other Google errors should be UNAVAILABLE, got %v", got)
	}
}

// The link is keyed on Google's sub, so editing the email on the Silo account
// (or a Silo outage) must not revoke access at the periodic re-check.
func TestCheckAccountIgnoresSiloEmailChanges(t *testing.T) {
	for name, silo := range map[string]*httptest.Server{
		"email no longer matches": fakeSilo(t, 200, map[string]bool{}),
		"silo outage":             fakeSilo(t, 500, nil),
	} {
		t.Run(name, func(t *testing.T) {
			p, f := setup(t, baseCfg(), silo)
			rs, _ := structpb.NewStruct(map[string]any{"refresh_token": "rt-1"})
			f.claims = func(string) map[string]any { return goodClaims("") }
			r, _ := p.CheckAccount(context.Background(), &pluginv1.CheckAccountRequest{ExternalSubject: "1234", RefreshState: rs})
			if r.Status != pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_ACTIVE {
				t.Errorf("want ACTIVE, got %v", r.Status)
			}
		})
	}
}

func TestGoogleOutageIsProviderUnavailable(t *testing.T) {
	p, f := setup(t, baseCfg(), fakeSilo(t, 200, defaultUsers))
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) })
	init, _ := start(t, p)
	resp, err := p.ExchangeCode(context.Background(), &pluginv1.ExchangeCodeRequest{Code: "c", ProviderState: init.ProviderState})
	if err != nil || resp.ExternalSubject != "" || resp.Denial != pluginv1.AuthDenial_AUTH_DENIAL_PROVIDER_UNAVAILABLE {
		t.Fatalf("503 from Google must be PROVIDER_UNAVAILABLE: %+v %v", resp, err)
	}
}

func TestSiloFailureMessageNamesSilo(t *testing.T) {
	p, f := setup(t, baseCfg(), fakeSilo(t, 500, nil))
	r := signIn(t, p, f, nil)
	if !strings.Contains(r.DenialDetail, "Silo") || strings.Contains(strings.ToLower(r.DenialDetail), "google unreachable") {
		t.Errorf("misleading detail: %q", r.DenialDetail)
	}
}

func TestIDTokenEdgeCases(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	enc := func(m map[string]any) string { return jwt(t, m) }
	good := goodClaims("n")
	if _, err := parseIDToken(enc(good), testClient, "n", now); err != nil {
		t.Fatal(err)
	}
	arr := goodClaims("n")
	arr["aud"] = []string{testClient, "other"}
	for name, tok := range map[string]string{
		"aud array":  enc(arr),
		"no sub":     enc(func() map[string]any { m := goodClaims("n"); delete(m, "sub"); return m }()),
		"not a jwt":  "abc",
		"bad base64": "a.!!!.c",
		"not json":   "a." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".c",
	} {
		if _, err := parseIDToken(tok, testClient, "n", now); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestAuthorizePromptAllowlist(t *testing.T) {
	cfg := config{ClientID: testClient}
	u := authorizeURL(googleEndpoints, cfg, "r", "s", "c", "n", "bogus&evil=1", "me@x.com")
	if strings.Contains(u, "bogus") || strings.Contains(u, "evil") {
		t.Errorf("unknown prompt must be dropped: %s", u)
	}
	if !strings.Contains(u, "login_hint=me%40x.com") {
		t.Errorf("login_hint must be URL-encoded: %s", u)
	}
}

func TestHTTPSOnly(t *testing.T) {
	for in, want := range map[string]string{"https://x/p.png": "https://x/p.png", "http://x/p.png": "", "javascript:alert(1)": "", "https://": "", "": ""} {
		if got := httpsOnly(in); got != want {
			t.Errorf("httpsOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAssetRoute(t *testing.T) {
	r, _ := routeServer{}.Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/assets/google.svg"})
	if r.StatusCode != 200 || r.Headers["Content-Type"] != "image/svg+xml" || !strings.Contains(string(r.Body), "<svg") {
		t.Errorf("icon not served: %d %v", r.StatusCode, r.Headers)
	}
	if r, _ := (routeServer{}).Handle(context.Background(), &pluginv1.HandleHTTPRequest{Method: "GET", Path: "/assets/../secret"}); r.StatusCode != 404 {
		t.Errorf("other paths must 404, got %d", r.StatusCode)
	}
}

func TestTestConnection(t *testing.T) {
	p, f := setup(t, map[string]any{}, fakeSilo(t, 200, defaultUsers))
	run := func(cfg map[string]any) *pluginv1.AuthTestConnectionResponse {
		r, err := p.TestConnection(context.Background(), &pluginv1.AuthTestConnectionRequest{Config: entries(t, cfg)})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := run(map[string]any{}); r.Ok {
		t.Error("empty config must fail")
	}
	noKey := baseCfg()
	delete(noKey, "silo_api_key")
	if r := run(noKey); r.Ok {
		t.Error("a missing Silo API key must fail")
	}
	f.err = "invalid_grant" // client accepted, bogus code refused
	if r := run(baseCfg()); !r.Ok || len(r.Steps) != 4 {
		t.Errorf("good config should pass: %+v", r)
	}
	f.err = "invalid_client"
	r := run(baseCfg())
	if r.Ok || strings.Contains(r.Steps[len(r.Steps)-1].Message, "s3") {
		t.Errorf("bad client should fail without echoing secrets: %+v", r)
	}
	p.siloBase = fakeSilo(t, 500, nil).URL
	f.err = "invalid_grant"
	if r := run(baseCfg()); r.Ok {
		t.Error("an unreadable Silo API must fail the test")
	}
}

func TestManifestValid(t *testing.T) {
	m, err := manifest.Load(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Validate(m); err != nil {
		t.Fatal(err)
	}
}
