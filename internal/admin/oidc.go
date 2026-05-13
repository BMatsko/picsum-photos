package admin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type oidcDiscovery struct {
	Issuer              string `json:"issuer"`
	AuthorizationURL    string `json:"authorization_endpoint"`
	TokenURL            string `json:"token_endpoint"`
	JWKSURL             string `json:"jwks_uri"`
	EndSessionURL       string `json:"end_session_endpoint"`
}

type OIDC struct {
	Enabled               bool
	DiscoveryURL          string
	Issuer                string
	AuthURL               string
	TokenURL              string
	JWKSURL               string
	EndSessionEndpoint    string
	RedirectURL           string
	PostLogoutRedirectURL string
	ClientID              string
	ClientSecret          string
	Scopes                []string
	oauth2Config          *oauth2.Config
	verifier              *oidc.IDTokenVerifier
	httpClient            *http.Client
}

const (
	oidcStateCookieName  = "admin_oidc_state"
	oidcNonceCookieName  = "admin_oidc_nonce"
	oidcIDTokenCookieName = "admin_oidc_id_token"
)

func newOIDC(rootURL string) (*OIDC, error) {
	clientID := strings.TrimSpace(os.Getenv("PICSUM_OIDC_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("PICSUM_OIDC_CLIENT_SECRET"))

	if clientID == "" && clientSecret == "" && strings.TrimSpace(os.Getenv("PICSUM_OIDC_DISCOVERY_URL")) == "" && strings.TrimSpace(os.Getenv("PICSUM_OIDC_ISSUER_URL")) == "" {
		return nil, nil
	}
	if clientID == "" || clientSecret == "" {
		return nil, fmt.Errorf("PICSUM_OIDC_CLIENT_ID and PICSUM_OIDC_CLIENT_SECRET are required when OIDC is enabled")
	}

	redirectURL := strings.TrimSpace(os.Getenv("PICSUM_OIDC_REDIRECT_URL"))
	if redirectURL == "" {
		redirectURL = strings.TrimRight(rootURL, "/") + "/admin/auth/callback"
	}
	postLogoutRedirectURL := strings.TrimSpace(os.Getenv("PICSUM_OIDC_POST_LOGOUT_REDIRECT_URL"))
	if postLogoutRedirectURL == "" {
		postLogoutRedirectURL = strings.TrimRight(rootURL, "/") + "/admin/login"
	}

	scopes := parseOIDCScopes(os.Getenv("PICSUM_OIDC_SCOPES"))
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}

	discoveryURL := strings.TrimSpace(os.Getenv("PICSUM_OIDC_DISCOVERY_URL"))
	issuerURL := strings.TrimSpace(os.Getenv("PICSUM_OIDC_ISSUER_URL"))

	candidates := []string{}
	if discoveryURL != "" {
		candidates = append(candidates, discoveryURL)
	}
	if issuerURL != "" {
		base := strings.TrimRight(issuerURL, "/")
		candidates = append(candidates,
			base+"/application/o/.well-known/openid-configuration",
			base+"/.well-known/openid-configuration",
		)
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	disc, usedURL, err := fetchOIDCDiscovery(candidates)
	if err != nil {
		return nil, err
	}

	if disc.Issuer == "" || disc.AuthorizationURL == "" || disc.TokenURL == "" || disc.JWKSURL == "" {
		return nil, fmt.Errorf("OIDC discovery document from %s is missing required fields", usedURL)
	}

	httpClient := &http.Client{Timeout: 10 * time.Second}
	ctx := context.Background()
	verifier := oidc.NewVerifier(disc.Issuer, oidc.NewRemoteKeySet(ctx, disc.JWKSURL), &oidc.Config{ClientID: clientID})

	return &OIDC{
		Enabled:               true,
		DiscoveryURL:          usedURL,
		Issuer:                disc.Issuer,
		AuthURL:               disc.AuthorizationURL,
		TokenURL:              disc.TokenURL,
		JWKSURL:               disc.JWKSURL,
		EndSessionEndpoint:    disc.EndSessionURL,
		RedirectURL:           redirectURL,
		PostLogoutRedirectURL: postLogoutRedirectURL,
		ClientID:              clientID,
		ClientSecret:          clientSecret,
		Scopes:                scopes,
		oauth2Config: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Scopes:       scopes,
			Endpoint: oauth2.Endpoint{
				AuthURL:  disc.AuthorizationURL,
				TokenURL: disc.TokenURL,
			},
		},
		verifier:   verifier,
		httpClient: httpClient,
	}, nil
}

func fetchOIDCDiscovery(candidates []string) (*oidcDiscovery, string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	var errs []string
	for _, rawURL := range candidates {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", rawURL, err))
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", rawURL, err))
			continue
		}
		var disc oidcDiscovery
		func() {
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode > 299 {
				errs = append(errs, fmt.Sprintf("%s: status %s", rawURL, resp.Status))
				return
			}
			if err := json.NewDecoder(resp.Body).Decode(&disc); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", rawURL, err))
				return
			}
		}()
		if disc.AuthorizationURL == "" || disc.TokenURL == "" || disc.JWKSURL == "" || disc.Issuer == "" {
			if len(errs) == 0 || !strings.Contains(errs[len(errs)-1], rawURL) {
				errs = append(errs, fmt.Sprintf("%s: incomplete discovery document", rawURL))
			}
			continue
		}
		return &disc, rawURL, nil
	}
	if len(errs) == 0 {
		return nil, "", errors.New("no OIDC discovery URL candidates were provided")
	}
	return nil, "", fmt.Errorf("failed to load OIDC discovery document: %s", strings.Join(errs, " | "))
}

func parseOIDCScopes(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func randomOIDCString(n int) (string, error) {
	if n <= 0 {
		n = 32
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (a *Admin) oidcEnabled() bool {
	return a != nil && a.OIDC != nil && a.OIDC.Enabled
}

func (a *Admin) cookieSecure() bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(a.RootURL)), "https://")
}

func (a *Admin) setAdminCookie(w http.ResponseWriter, name, value string, maxAge int, expires time.Time, path string) {
	cookie := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.cookieSecure(),
	}
	if maxAge != 0 {
		cookie.MaxAge = maxAge
	}
	if !expires.IsZero() {
		cookie.Expires = expires
	}
	http.SetCookie(w, cookie)
}

func (a *Admin) clearAdminCookie(w http.ResponseWriter, name, path string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     path,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   a.cookieSecure(),
	})
}

func (a *Admin) oidcSessionCookie(r *http.Request) (string, error) {
	cookie, err := r.Cookie(oidcIDTokenCookieName)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(cookie.Value) == "" {
		return "", errors.New("empty oidc session cookie")
	}
	return cookie.Value, nil
}

func (a *Admin) oidcLogoutURL(r *http.Request) (string, bool) {
	if !a.oidcEnabled() || a.OIDC.EndSessionEndpoint == "" {
		return "", false
	}
	idToken, err := a.oidcSessionCookie(r)
	if err != nil {
		return "", false
	}
	logoutURL, err := url.Parse(a.OIDC.EndSessionEndpoint)
	if err != nil {
		return "", false
	}
	q := logoutURL.Query()
	q.Set("id_token_hint", idToken)
	if strings.TrimSpace(a.OIDC.PostLogoutRedirectURL) != "" {
		q.Set("post_logout_redirect_uri", a.OIDC.PostLogoutRedirectURL)
	}
	logoutURL.RawQuery = q.Encode()
	return logoutURL.String(), true
}

func (a *Admin) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if !a.oidcEnabled() {
		http.Redirect(w, r, "/admin/login?error=OIDC+is+not+configured", http.StatusFound)
		return
	}
	state, err := randomOIDCString(32)
	if err != nil {
		http.Redirect(w, r, "/admin/login?error=Failed+to+start+OIDC+login", http.StatusFound)
		return
	}
	nonce, err := randomOIDCString(32)
	if err != nil {
		http.Redirect(w, r, "/admin/login?error=Failed+to+start+OIDC+login", http.StatusFound)
		return
	}
	a.setAdminCookie(w, oidcStateCookieName, state, 300, time.Time{}, "/admin/auth")
	a.setAdminCookie(w, oidcNonceCookieName, nonce, 300, time.Time{}, "/admin/auth")
	loginURL := a.OIDC.oauth2Config.AuthCodeURL(state, oauth2.SetAuthURLParam("nonce", nonce))
	http.Redirect(w, r, loginURL, http.StatusFound)
}

func (a *Admin) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if !a.oidcEnabled() {
		http.Redirect(w, r, "/admin/login?error=OIDC+is+not+configured", http.StatusFound)
		return
	}
	if errParam := strings.TrimSpace(r.URL.Query().Get("error")); errParam != "" {
		msg := errParam
		if desc := strings.TrimSpace(r.URL.Query().Get("error_description")); desc != "" {
			msg = msg + ": " + desc
		}
		http.Redirect(w, r, "/admin/login?error="+url.QueryEscape(msg), http.StatusFound)
		return
	}
	stateCookie, err := r.Cookie(oidcStateCookieName)
	if err != nil || stateCookie.Value == "" || stateCookie.Value != r.URL.Query().Get("state") {
		http.Redirect(w, r, "/admin/login?error=Invalid+OIDC+state", http.StatusFound)
		return
	}
	nonceCookie, err := r.Cookie(oidcNonceCookieName)
	if err != nil || nonceCookie.Value == "" {
		http.Redirect(w, r, "/admin/login?error=Missing+OIDC+nonce", http.StatusFound)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		http.Redirect(w, r, "/admin/login?error=Missing+OIDC+code", http.StatusFound)
		return
	}
	token, err := a.OIDC.oauth2Config.Exchange(r.Context(), code)
	if err != nil {
		http.Redirect(w, r, "/admin/login?error=OIDC+token+exchange+failed", http.StatusFound)
		return
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	if rawIDToken == "" {
		http.Redirect(w, r, "/admin/login?error=OIDC+did+not+return+an+id_token", http.StatusFound)
		return
	}
	idToken, err := a.OIDC.verifier.Verify(r.Context(), rawIDToken)
	if err != nil {
		http.Redirect(w, r, "/admin/login?error=OIDC+token+verification+failed", http.StatusFound)
		return
	}
	if idToken.Nonce != nonceCookie.Value {
		http.Redirect(w, r, "/admin/login?error=OIDC+nonce+verification+failed", http.StatusFound)
		return
	}
	a.setAdminCookie(w, "admin_session", sessionToken, 0, time.Time{}, "/admin")
	a.setAdminCookie(w, oidcIDTokenCookieName, rawIDToken, 0, idToken.Expiry, "/admin")
	a.clearOIDCCookies(w)
	http.Redirect(w, r, "/admin", http.StatusFound)
}

func (a *Admin) clearOIDCCookies(w http.ResponseWriter) {
	a.clearAdminCookie(w, oidcStateCookieName, "/admin/auth")
	a.clearAdminCookie(w, oidcNonceCookieName, "/admin/auth")
}
