package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

type provider struct {
	pluginv1.UnimplementedAuthProviderServer
	pluginv1.UnimplementedAuthProviderChecksServer

	log hclog.Logger
	hc  *http.Client
	ep  endpoints
	now func() time.Time
	// siloBase is Silo's internal listener; empty means the default. Only tests set it.
	siloBase string

	mu  sync.RWMutex
	cfg config
}

var (
	_ pluginv1.AuthProviderServer       = (*provider)(nil)
	_ pluginv1.AuthProviderChecksServer = (*provider)(nil)
)

func newProvider(log hclog.Logger) *provider {
	return &provider{log: log, hc: &http.Client{Timeout: 20 * time.Second}, ep: googleEndpoints, now: time.Now}
}

func (p *provider) configure(entries []*pluginv1.ConfigEntry) {
	c := parseConfig(entries)
	p.mu.Lock()
	p.cfg = c
	p.mu.Unlock()
}

func (p *provider) config() config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

func denial(d pluginv1.AuthDenial, detail string) *pluginv1.AuthenticateResponse {
	// external_subject stays empty so older hosts also fail closed.
	return &pluginv1.AuthenticateResponse{Denial: d, DenialDetail: detail}
}

// Authenticate: this plugin serves OAuth only.
func (p *provider) Authenticate(context.Context, *pluginv1.AuthenticateRequest) (*pluginv1.AuthenticateResponse, error) {
	return denial(pluginv1.AuthDenial_AUTH_DENIAL_INVALID_CREDENTIALS, "password sign-in is not supported by the Google provider"), nil
}

func (p *provider) InitAuthorize(_ context.Context, in *pluginv1.InitAuthorizeRequest) (*pluginv1.InitAuthorizeResponse, error) {
	c := p.config()
	if c.ClientID == "" || c.ClientSecret == "" {
		return nil, status.Error(codes.FailedPrecondition, "Google client ID and secret are not configured")
	}
	if in.GetRedirectUri() == "" || in.GetState() == "" {
		return nil, status.Error(codes.InvalidArgument, "redirect_uri and state are required")
	}
	verifier, err := randomString(48)
	if err != nil {
		return nil, status.Error(codes.Internal, "random: "+err.Error())
	}
	nonce, err := randomString(24)
	if err != nil {
		return nil, status.Error(codes.Internal, "random: "+err.Error())
	}
	st, err := structpb.NewStruct(map[string]any{"code_verifier": verifier, "nonce": nonce})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pluginv1.InitAuthorizeResponse{
		AuthorizeUrl:  authorizeURL(p.ep, c, in.GetRedirectUri(), in.GetState(), pkceChallenge(verifier), nonce, in.GetPrompt(), in.GetLoginHint()),
		ProviderState: st,
	}, nil
}

func (p *provider) ExchangeCode(ctx context.Context, in *pluginv1.ExchangeCodeRequest) (*pluginv1.AuthenticateResponse, error) {
	c := p.config()
	if c.ClientID == "" || c.ClientSecret == "" {
		return denial(pluginv1.AuthDenial_AUTH_DENIAL_PROVIDER_UNAVAILABLE, "Google client ID and secret are not configured"), nil
	}
	if c.SiloAPIKey == "" {
		return denial(pluginv1.AuthDenial_AUTH_DENIAL_PROVIDER_UNAVAILABLE, "the Silo API key is not configured, so Silo accounts cannot be checked"), nil
	}
	ps := in.GetProviderState().AsMap()
	verifier, _ := ps["code_verifier"].(string)
	nonce, _ := ps["nonce"].(string)
	if verifier == "" || nonce == "" {
		return denial(pluginv1.AuthDenial_AUTH_DENIAL_INVALID_CREDENTIALS, "missing PKCE state"), nil
	}
	tr, err := postToken(ctx, p.hc, p.ep, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {in.GetCode()},
		"redirect_uri":  {in.GetRedirectUri()},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"code_verifier": {verifier},
	})
	if err != nil {
		if errors.Is(err, errTransport) {
			return denial(pluginv1.AuthDenial_AUTH_DENIAL_PROVIDER_UNAVAILABLE, err.Error()), nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	if tr.Error != "" {
		switch tr.Error {
		case "invalid_grant":
			return denial(pluginv1.AuthDenial_AUTH_DENIAL_INVALID_CREDENTIALS, "Google rejected the code: "+tr.ErrorDesc), nil
		default: // invalid_client, redirect_uri_mismatch, ...
			return denial(pluginv1.AuthDenial_AUTH_DENIAL_PROVIDER_UNAVAILABLE, "Google token error "+tr.Error+": "+tr.ErrorDesc), nil
		}
	}
	claims, err := parseIDToken(tr.IDToken, c.ClientID, nonce, p.now())
	if err != nil {
		return denial(pluginv1.AuthDenial_AUTH_DENIAL_INVALID_CREDENTIALS, err.Error()), nil
	}
	return p.signIn(ctx, c, claims, tr.RefreshToken), nil
}

// signIn applies the access rule (a verified Google email that belongs to an
// enabled Silo account) and builds the response.
func (p *provider) signIn(ctx context.Context, c config, cl idClaims, refresh string) *pluginv1.AuthenticateResponse {
	if !cl.EmailVerified {
		return denial(pluginv1.AuthDenial_AUTH_DENIAL_NOT_PERMITTED, "Google has not verified the email address")
	}
	if c.SiloAPIKey == "" {
		return denial(pluginv1.AuthDenial_AUTH_DENIAL_PROVIDER_UNAVAILABLE, "the Silo API key is not configured, so Silo accounts cannot be checked")
	}
	ok, err := newSiloClient(p.siloBase, c.SiloAPIKey, p.hc).accountByEmail(ctx, cl.Email)
	if err != nil {
		p.log.Warn("silo account lookup failed", "error", err.Error())
		return denial(pluginv1.AuthDenial_AUTH_DENIAL_PROVIDER_UNAVAILABLE, "could not look up Silo accounts: "+err.Error())
	}
	if !ok {
		p.log.Info("sign-in refused: no enabled Silo account", "email", cl.Email)
		return denial(pluginv1.AuthDenial_AUTH_DENIAL_NOT_PERMITTED, cl.Email+" has no enabled Silo account")
	}
	return accountResponse(cl, refresh)
}

// accountResponse maps verified Google claims to the host's identity. The role
// is deliberately never set: Silo keeps the one the account already has.
func accountResponse(cl idClaims, refresh string) *pluginv1.AuthenticateResponse {
	verified := cl.EmailVerified
	resp := &pluginv1.AuthenticateResponse{
		ExternalSubject: cl.Subject, // Google's immutable account ID, never the email
		DisplayName:     cl.Name,
		Email:           cl.Email,
		Issuer:          "https://accounts.google.com",
		Username:        localPart(cl.Email),
		EmailVerified:   &verified,
		PictureUrl:      httpsOnly(cl.Picture),
	}
	if refresh != "" {
		if st, err := structpb.NewStruct(map[string]any{"refresh_token": refresh}); err == nil {
			resp.RefreshState = st
		}
	}
	return resp
}

func (p *provider) TestConnection(ctx context.Context, in *pluginv1.AuthTestConnectionRequest) (*pluginv1.AuthTestConnectionResponse, error) {
	c := parseConfig(in.GetConfig())
	out := &pluginv1.AuthTestConnectionResponse{Ok: true}
	add := func(id, label string, ok bool, msg string) bool {
		out.Steps = append(out.Steps, &pluginv1.AuthTestStep{Id: id, Label: label, Ok: ok, Message: msg})
		if !ok {
			out.Ok = false
		}
		return ok
	}
	if c.ClientID == "" || c.ClientSecret == "" {
		add("settings", "Client ID and secret set", false, "Both the client ID and the client secret are required.")
		return out, nil
	}
	add("settings", "Client ID and secret set", true, "")
	if c.SiloAPIKey == "" {
		add("silo", "Silo accounts readable", false, "A Silo API key is required: it is how the plugin checks who has an account.")
		return out, nil
	}
	if _, err := newSiloClient(p.siloBase, c.SiloAPIKey, p.hc).accountByEmail(ctx, "connection-test@invalid"); err != nil {
		add("silo", "Silo accounts readable", false, err.Error())
		return out, nil
	}
	add("silo", "Silo accounts readable", true, "")
	if err := p.checkDiscovery(ctx); err != nil {
		add("discovery", "Google reachable", false, err.Error())
		return out, nil
	}
	add("discovery", "Google reachable", true, "")
	// A deliberately bad code: Google answers invalid_client for wrong
	// credentials and invalid_grant when the client itself is accepted.
	tr, err := postToken(ctx, p.hc, p.ep, url.Values{
		"grant_type": {"authorization_code"}, "code": {"connection-test"}, "redirect_uri": {"https://localhost/"},
		"client_id": {c.ClientID}, "client_secret": {c.ClientSecret},
	})
	switch {
	case err != nil:
		add("client", "Client credentials accepted", false, err.Error())
	case tr.Error == "invalid_client":
		add("client", "Client credentials accepted", false, "Google rejected the client ID or secret.")
	default:
		add("client", "Client credentials accepted", true, "")
	}
	return out, nil
}

func (p *provider) checkDiscovery(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.ep.Discovery, nil)
	if err != nil {
		return err
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("discovery document returned " + resp.Status)
	}
	return nil
}

func (p *provider) CheckAccount(ctx context.Context, in *pluginv1.CheckAccountRequest) (*pluginv1.CheckAccountResponse, error) {
	c := p.config()
	rt, _ := in.GetRefreshState().AsMap()["refresh_token"].(string)
	if rt == "" || c.ClientID == "" {
		return &pluginv1.CheckAccountResponse{Status: pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_UNSUPPORTED}, nil
	}
	tr, err := postToken(ctx, p.hc, p.ep, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {rt},
		"client_id": {c.ClientID}, "client_secret": {c.ClientSecret},
	})
	if err != nil {
		if errors.Is(err, errTransport) {
			return &pluginv1.CheckAccountResponse{Status: pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_UNAVAILABLE}, nil
		}
		return nil, status.Error(codes.Internal, err.Error())
	}
	if tr.Error != "" {
		// invalid_grant means expired or revoked and we can't tell which
		// (Google test-mode apps expire tokens after 7 days), so UNSUPPORTED.
		if tr.Error == "invalid_grant" {
			return &pluginv1.CheckAccountResponse{Status: pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_UNSUPPORTED}, nil
		}
		return &pluginv1.CheckAccountResponse{Status: pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_UNAVAILABLE}, nil
	}
	cl, err := parseIDToken(tr.IDToken, c.ClientID, "", p.now())
	if err != nil || cl.Subject != in.GetExternalSubject() {
		return &pluginv1.CheckAccountResponse{Status: pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_UNAVAILABLE}, nil
	}
	// Whether the Silo account is still enabled is Silo's own business: it
	// already revokes the sessions of a disabled or deleted account. Re-running
	// the email lookup here would revoke access when someone merely edits the
	// email on their Silo account, because the link is keyed on Google's sub.
	if !cl.EmailVerified {
		return &pluginv1.CheckAccountResponse{Status: pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_NOT_PERMITTED}, nil
	}
	acct := accountResponse(cl, tr.RefreshToken)
	return &pluginv1.CheckAccountResponse{Status: pluginv1.CheckAccountStatus_CHECK_ACCOUNT_STATUS_ACTIVE, Account: acct}, nil
}
