// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package jwtauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hashicorp/cap/oidc"
	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/go-secure-stdlib/strutil"
	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/helper/cidrutil"
	"github.com/hashicorp/vault/sdk/logical"
	"golang.org/x/oauth2"
)

const (
	oidcRequestTimeout         = 10 * time.Minute
	oidcRequestCleanupInterval = 1 * time.Minute
)

const (
	// OIDC error prefixes. These are searched for specifically by the UI, so any
	// changes to them must be aligned with a UI change.
	errLoginFailed       = "Vault login failed."
	errNoResponse        = "No response from provider."
	errTokenVerification = "Token verification failed."
	errNotOIDCFlow       = "OIDC login is not configured for this mount"

	noCode = "no_code"
)

// oidcRequest represents a single OIDC authentication flow. It is created when
// an authURL is requested. It is uniquely identified by a state, which is passed
// throughout the multiple interactions needed to complete the flow.
type oidcRequest struct {
	oidc.Request

	rolename string
	code     string
	idToken  string

	// clientNonce is used between Vault and the client/application (e.g. CLI) making the request,
	// and is unrelated to the OIDC nonce above. It is optional.
	clientNonce string

	// this is for storing the response in direct callback mode
	auth *logical.Auth
}

func pathOIDC(b *jwtAuthBackend) []*framework.Path {
	return []*framework.Path{
		{
			Pattern: `oidc/callback`,
			Fields: map[string]*framework.FieldSchema{
				"state": {
					Type: framework.TypeString,
				},
				"code": {
					Type: framework.TypeString,
				},
				"id_token": {
					Type: framework.TypeString,
				},
				"client_nonce": {
					Type: framework.TypeString,
				},
				"error_description": {
					Type: framework.TypeString,
				},
			},

			Operations: map[logical.Operation]framework.OperationHandler{
				logical.ReadOperation: &framework.PathOperation{
					Callback: b.pathCallback,
					Summary:  "Callback endpoint to complete an OIDC login.",

					// state is cached so don't process OIDC logins on perf standbys
					ForwardPerformanceStandby: true,
				},
				logical.UpdateOperation: &framework.PathOperation{
					Callback: b.pathCallbackPost,
					Summary:  "Callback endpoint to handle form_posts.",

					// state is cached so don't process OIDC logins on perf standbys
					ForwardPerformanceStandby: true,
				},
			},
		},
		{
			Pattern: `oidc/poll`,
			Fields: map[string]*framework.FieldSchema{
				"state": {
					Type: framework.TypeString,
				},
				"client_nonce": {
					Type: framework.TypeString,
				},
			},
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback: b.pathPoll,
					Summary:  "Poll endpoint to complete an OIDC login.",

					// state is cached so don't process OIDC logins on perf standbys
					ForwardPerformanceStandby: true,
				},
			},
		},
		{
			Pattern: `oidc/auth_url`,
			Fields: map[string]*framework.FieldSchema{
				"role": {
					Type:        framework.TypeLowerCaseString,
					Description: "The role to issue an OIDC authorization URL against.",
				},
				"redirect_uri": {
					Type:        framework.TypeString,
					Description: "The OAuth redirect_uri to use in the authorization URL.",
				},
				"client_nonce": {
					Type:        framework.TypeString,
					Description: "Client-provided nonce that must match during callback, if present. Required only in direct callback mode.",
				},
			},
			Operations: map[logical.Operation]framework.OperationHandler{
				logical.UpdateOperation: &framework.PathOperation{
					Callback: b.authURL,
					Summary:  "Request an authorization URL to start an OIDC login flow.",

					// state is cached so don't process OIDC logins on perf standbys
					ForwardPerformanceStandby: true,
				},
			},
		},
	}
}

func (b *jwtAuthBackend) pathCallbackPost(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	config, err := b.config(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return logical.ErrorResponse(errLoginFailed + " Could not load configuration."), nil
	}

	if config.OIDCResponseMode != responseModeFormPost {
		return logical.RespondWithStatusCode(nil, req, http.StatusMethodNotAllowed)
	}

	stateID := d.Get("state").(string)
	code := d.Get("code").(string)
	idToken := d.Get("id_token").(string)

	resp := &logical.Response{
		Data: map[string]interface{}{
			logical.HTTPContentType: "text/html",
			logical.HTTPStatusCode:  http.StatusOK,
		},
	}

	// Store the provided code and/or token into its OIDC request, which must already exist.
	oidcReq := b.getOIDCRequest(stateID)
	if oidcReq == nil {
		resp.Data[logical.HTTPRawBody] = []byte(errorHTML(errLoginFailed, "Expired or missing OAuth state."))
		resp.Data[logical.HTTPStatusCode] = http.StatusBadRequest
	} else {
		oidcReq.code = code
		oidcReq.idToken = idToken
		b.setOIDCRequest(stateID, oidcReq)
		mount := parseMount(oidcReq.RedirectURL())
		if mount == "" {
			resp.Data[logical.HTTPRawBody] = []byte(errorHTML(errLoginFailed, "Invalid redirect path."))
			resp.Data[logical.HTTPStatusCode] = http.StatusBadRequest
		} else {
			resp.Data[logical.HTTPRawBody] = []byte(formpostHTML(mount, noCode, stateID))
		}
	}

	return resp, nil
}

func loginFailedResponse(useHttp bool, msg string) *logical.Response {
	if !useHttp {
		return logical.ErrorResponse(errLoginFailed + " " + msg)
	}
	return &logical.Response{
		Data: map[string]interface{}{
			logical.HTTPContentType: "text/html",
			logical.HTTPStatusCode:  http.StatusBadRequest,
			logical.HTTPRawBody:     []byte(errorHTML(errLoginFailed, msg)),
		},
	}
}

func (b *jwtAuthBackend) pathCallback(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	config, err := b.config(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return logical.ErrorResponse(errLoginFailed + " Could not load configuration"), nil
	}

	stateID := d.Get("state").(string)

	oidcReq := b.getOIDCRequest(stateID)
	if oidcReq == nil || oidcReq.auth != nil {
		return logical.ErrorResponse(errLoginFailed + " Expired or missing OAuth state."), nil
	}

	roleName := oidcReq.rolename
	role, err := b.role(ctx, req.Storage, roleName)
	if err != nil {
		b.deleteOIDCRequest(stateID)
		return nil, err
	}
	if role == nil {
		b.deleteOIDCRequest(stateID)
		return logical.ErrorResponse(errLoginFailed + " Role could not be found"), nil
	}

	useHttp := false
	if role.CallbackMode == callbackModeDirect {
		useHttp = true
	}
	if !useHttp {
		// state is only accessed once when not using direct callback
		b.deleteOIDCRequest(stateID)
	}

	errorDescription := d.Get("error_description").(string)
	if errorDescription != "" {
		return loginFailedResponse(useHttp, errorDescription), nil
	}

	clientNonce := d.Get("client_nonce").(string)

	// If a client_nonce was provided at the start of the auth process as part of the auth_url
	// request, require that it is present and matching during the callback phase
	// unless using the direct callback mode (when we instead check in poll).
	if oidcReq.clientNonce != "" && clientNonce != oidcReq.clientNonce && !useHttp {
		return logical.ErrorResponse("invalid client_nonce"), nil
	}

	if len(role.TokenBoundCIDRs) > 0 {
		if req.Connection == nil {
			b.Logger().Warn("token bound CIDRs found but no connection information available for validation")
			return nil, logical.ErrPermissionDenied
		}
		if !cidrutil.RemoteAddrIsOk(req.Connection.RemoteAddr, role.TokenBoundCIDRs) {
			return nil, logical.ErrPermissionDenied
		}
	}

	provider, err := b.getProvider(config)
	if err != nil {
		return nil, errwrap.Wrapf("error getting provider for login operation: {{err}}", err)
	}

	var rawToken oidc.IDToken
	var token *oidc.Tk

	code := d.Get("code").(string)
	if code == noCode {
		code = oidcReq.code
	}

	if code == "" {
		if oidcReq.idToken == "" {
			return loginFailedResponse(useHttp, "No code or id_token received."), nil
		}

		// Verify the ID token received from the authentication response.
		rawToken = oidc.IDToken(oidcReq.idToken)
		if _, err := provider.VerifyIDToken(ctx, rawToken, oidcReq); err != nil {
			return logical.ErrorResponse("%s %s", errTokenVerification, err.Error()), nil
		}
	} else {
		// Exchange the authorization code for an ID token and access token.
		// ID token verification takes place in provider.Exchange.
		token, err = provider.Exchange(ctx, oidcReq, stateID, code)
		if err != nil {
			return loginFailedResponse(useHttp, fmt.Sprintf("Error exchanging oidc code: %q.", err.Error())), nil
		}

		rawToken = token.IDToken()
	}

	if role.VerboseOIDCLogging {
		loggedToken := "invalid token format"

		parts := strings.Split(string(rawToken), ".")
		if len(parts) == 3 {
			// strip signature from logged token
			loggedToken = fmt.Sprintf("%s.%s.xxxxxxxxxxx", parts[0], parts[1])
		}

		b.Logger().Debug("OIDC provider response", "id_token", loggedToken)
	}

	// Parse claims from the ID token payload.
	var allClaims map[string]interface{}
	if err := rawToken.Claims(&allClaims); err != nil {
		return nil, err
	}
	delete(allClaims, "nonce")

	// Get the subject claim for bound subject and user info validation
	var subject string
	if subStr, ok := allClaims["sub"].(string); ok {
		subject = subStr
	}

	if role.BoundSubject != "" && role.BoundSubject != subject {
		return loginFailedResponse(useHttp, "sub claim does not match bound subject"), nil
	}

	// Set the token source for the access token if it's available. It will only
	// be available for the authorization code flow (oidc_response_types=code).
	// The access token will be used for fetching additional user and group info.
	var tokenSource oauth2.TokenSource
	if token != nil {
		tokenSource = token.StaticTokenSource()
	}

	// If we have a token, attempt to fetch information from the /userinfo endpoint
	// and merge it with the existing claims data. A failure to fetch additional information
	// from this endpoint will not invalidate the authorization flow.
	if tokenSource != nil {
		if err := provider.UserInfo(ctx, tokenSource, subject, &allClaims); err != nil {
			logFunc := b.Logger().Warn
			if strings.Contains(err.Error(), "user info endpoint is not supported") {
				logFunc = b.Logger().Info
			}
			logFunc("error reading /userinfo endpoint", "error", err)
		}
	}

	if role.VerboseOIDCLogging {
		if c, err := json.Marshal(allClaims); err == nil {
			b.Logger().Debug("OIDC provider response", "claims", string(c))
		} else {
			b.Logger().Debug("OIDC provider response", "marshalling error", err.Error())
		}
	}

	alias, groupAliases, err := b.createIdentity(ctx, allClaims, role, tokenSource)
	if err != nil {
		return loginFailedResponse(useHttp, err.Error()), nil
	}

	if err := validateBoundClaims(b.Logger(), role.BoundClaimsType, role.BoundClaims, allClaims); err != nil {
		return loginFailedResponse(useHttp, fmt.Sprintf("error validating claims: %s", err.Error())), nil
	}

	tokenMetadata := map[string]string{"role": roleName}
	for k, v := range alias.Metadata {
		tokenMetadata[k] = v
	}

	auth := &logical.Auth{
		Policies:     role.Policies,
		DisplayName:  alias.Name,
		Period:       role.Period,
		NumUses:      role.NumUses,
		Alias:        alias,
		GroupAliases: groupAliases,
		InternalData: map[string]interface{}{
			"role": roleName,
		},
		Metadata: tokenMetadata,
		LeaseOptions: logical.LeaseOptions{
			Renewable: true,
			TTL:       role.TTL,
			MaxTTL:    role.MaxTTL,
		},
		BoundCIDRs: role.BoundCIDRs,
	}

	role.PopulateTokenAuth(auth)

	resp := &logical.Response{}
	if useHttp {
		oidcReq.auth = auth
		b.setOIDCRequest(stateID, oidcReq)
		resp.Data = map[string]interface{}{
			logical.HTTPContentType: "text/html",
			logical.HTTPStatusCode:  http.StatusOK,
			logical.HTTPRawBody:     []byte(successHTML),
		}
	} else {
		resp.Auth = auth
	}

	return resp, nil
}

func (b *jwtAuthBackend) pathPoll(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	stateID := d.Get("state").(string)

	oidcReq := b.getOIDCRequest(stateID)
	if oidcReq == nil {
		return logical.ErrorResponse(errLoginFailed + " Expired or missing OAuth state."), nil
	}

	clientNonce := d.Get("client_nonce").(string)

	if oidcReq.clientNonce != "" && clientNonce != oidcReq.clientNonce {
		b.deleteOIDCRequest(stateID)
		return logical.ErrorResponse("invalid client_nonce"), nil
	}

	if oidcReq.auth == nil {
		// Return the same response as oauth 2.0 device flow in RFC8628
		return logical.ErrorResponse("authorization_pending"), nil
	}

	b.deleteOIDCRequest(stateID)
	resp := &logical.Response{
		Auth: oidcReq.auth,
	}
	return resp, nil
}

// authURL returns a URL used for redirection to receive an authorization code.
// This path requires a role name, or that a default_role has been configured.
// Because this endpoint is unauthenticated, the response to invalid or non-OIDC
// roles is intentionally non-descriptive and will simply be an empty string.
func (b *jwtAuthBackend) authURL(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	logger := b.Logger()

	// default response for most error/invalid conditions
	resp := &logical.Response{
		Data: map[string]interface{}{
			"auth_url": "",
		},
	}

	config, err := b.config(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if config == nil {
		return logical.ErrorResponse("could not load configuration"), nil
	}

	if config.authType() != OIDCFlow {
		return logical.ErrorResponse(errNotOIDCFlow), nil
	}

	roleName := d.Get("role").(string)
	if roleName == "" {
		roleName = config.DefaultRole
	}
	if roleName == "" {
		return logical.ErrorResponse("missing role"), nil
	}

	redirectURI := d.Get("redirect_uri").(string)
	if redirectURI == "" {
		return logical.ErrorResponse("missing redirect_uri"), nil
	}

	role, err := b.role(ctx, req.Storage, roleName)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return logical.ErrorResponse("role %q could not be found", roleName), nil
	}

	clientNonce := d.Get("client_nonce").(string)
	if clientNonce == "" && role.CallbackMode != callbackModeClient {
		return logical.ErrorResponse("missing client_nonce"), nil
	}

	// If namespace will be passed around in oidcReq, and it has been provided as
	// a redirectURI query parameter, remove it from redirectURI, and append it
	// to the oidcReq (later in this function)
	namespace := ""
	if config.NamespaceInState {
		inputURI, err := url.Parse(redirectURI)
		if err != nil {
			return resp, nil
		}
		qParam := inputURI.Query()
		namespace = qParam.Get("namespace")
		if len(namespace) > 0 {
			qParam.Del("namespace")
			inputURI.RawQuery = qParam.Encode()
			redirectURI = inputURI.String()
		}
	}

	if !validRedirect(redirectURI, role.AllowedRedirectURIs) {
		logger.Warn("unauthorized redirect_uri", "redirect_uri", redirectURI)
		return resp, nil
	}

	// If configured for form_post, redirect directly to Vault instead of the UI,
	// if this was initiated by the UI (which currently has no knowledge of mode).
	//
	// TODO: it would be better to convey this to the UI and have it send the
	// correct URL directly.
	if config.OIDCResponseMode == responseModeFormPost {
		redirectURI = strings.Replace(redirectURI, "ui/vault", "v1", 1)
	}

	provider, err := b.getProvider(config)
	if err != nil {
		logger.Warn("error getting provider for login operation", "error", err)
		return resp, nil
	}

	oidcReq, err := b.createOIDCRequest(config, role, roleName, redirectURI, clientNonce)
	if err != nil {
		logger.Warn("error generating OAuth state", "error", err)
		return resp, nil
	}

	urlStr, err := provider.AuthURL(ctx, oidcReq)
	if err != nil {
		logger.Warn("error generating auth URL", "error", err)
		return resp, nil
	}

	// embed namespace in oidcReq in the auth_url
	if config.NamespaceInState && len(namespace) > 0 {
		stateWithNamespace := fmt.Sprintf("%s,ns=%s", oidcReq.State(), namespace)
		urlStr = strings.Replace(urlStr, oidcReq.State(), url.QueryEscape(stateWithNamespace), 1)
	}

	resp.Data["auth_url"] = urlStr
	if role.CallbackMode == callbackModeDirect {
		resp.Data["state"] = oidcReq.State()
		resp.Data["poll_interval"] = "5"
	}

	return resp, nil
}

// createOIDCRequest makes an expiring request object, associated with a random state ID
// that is passed throughout the OAuth process. A nonce is also included in the auth process.
func (b *jwtAuthBackend) createOIDCRequest(config *jwtConfig, role *jwtRole, rolename, redirectURI, clientNonce string) (*oidcRequest, error) {
	options := []oidc.Option{
		oidc.WithAudiences(role.BoundAudiences...),
		oidc.WithScopes(role.OIDCScopes...),
	}

	if config.hasType(responseTypeIDToken) {
		options = append(options, oidc.WithImplicitFlow())
	} else if config.hasType(responseTypeCode) {
		v, err := oidc.NewCodeVerifier()
		if err != nil {
			return nil, fmt.Errorf("error creating code challenge: %w", err)
		}

		options = append(options, oidc.WithPKCE(v))
	}

	if role.MaxAge > 0 {
		options = append(options, oidc.WithMaxAge(uint(role.MaxAge.Seconds())))
	}

	request, err := oidc.NewRequest(oidcRequestTimeout, redirectURI, options...)
	if err != nil {
		return nil, err
	}

	oidcReq := &oidcRequest{
		Request:     request,
		rolename:    rolename,
		clientNonce: clientNonce,
	}
	b.oidcRequests.SetDefault(request.State(), oidcReq)

	return oidcReq, nil
}

func (b *jwtAuthBackend) setOIDCRequest(stateID string, oidcReq *oidcRequest) {
	b.oidcRequests.SetDefault(stateID, oidcReq)
}

func (b *jwtAuthBackend) getOIDCRequest(stateID string) *oidcRequest {
	if requestRaw, ok := b.oidcRequests.Get(stateID); ok {
		return requestRaw.(*oidcRequest)
	}
	return nil
}

func (b *jwtAuthBackend) deleteOIDCRequest(stateID string) {
	b.oidcRequests.Delete(stateID)
}

// validRedirect checks whether uri is in allowed using special handling for loopback uris.
// Ref: https://tools.ietf.org/html/rfc8252#section-7.3
func validRedirect(uri string, allowed []string) bool {
	inputURI, err := url.Parse(uri)
	if err != nil {
		return false
	}

	// if uri isn't a loopback, just string search the allowed list
	if !strutil.StrListContains([]string{"localhost", "127.0.0.1", "::1"}, inputURI.Hostname()) {
		return strutil.StrListContains(allowed, uri)
	}

	// otherwise, search for a match in a port-agnostic manner, per the OAuth RFC.
	inputURI.Host = inputURI.Hostname()

	for _, a := range allowed {
		allowedURI, err := url.Parse(a)
		if err != nil {
			return false
		}
		allowedURI.Host = allowedURI.Hostname()

		if inputURI.String() == allowedURI.String() {
			return true
		}
	}

	return false
}

// parseMount attempts to extract the mount path from a redirect URI.
func parseMount(redirectURI string) string {
	parts := strings.Split(redirectURI, "/")

	for i := 0; i+2 < len(parts); i++ {
		if parts[i] == "v1" && parts[i+1] == "auth" {
			return parts[i+2]
		}
	}
	return ""
}
