package handler

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/oauth"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/imroc/req/v3"
	"github.com/tidwall/gjson"
)

const (
	oxsciOAuthCookiePath        = "/api/v1/auth/oauth/oxsci"
	oxsciOAuthStateCookieName   = "oxsci_oauth_state"
	oxsciOAuthVerifierCookie    = "oxsci_oauth_verifier"
	oxsciOAuthRedirectCookie    = "oxsci_oauth_redirect"
	oxsciOAuthCookieMaxAgeSec   = 10 * 60 // 10 minutes
	oxsciOAuthDefaultRedirectTo = "/dashboard"
	oxsciOAuthDefaultFrontendCB = "/auth/oxsci/callback"

	oxsciOAuthMaxRedirectLen      = 2048
	oxsciOAuthMaxFragmentValueLen = 512
	oxsciOAuthMaxSubjectLen       = 64 - len("oxsci-")
)

// OxSciConnectSyntheticEmailDomain 是 OxSci OAuth 用户的合成邮箱后缀。
const OxSciConnectSyntheticEmailDomain = "@oxsci-connect.invalid"

type oxsciTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

type oxsciTokenExchangeError struct {
	StatusCode          int
	ProviderError       string
	ProviderDescription string
	Body                string
}

func (e *oxsciTokenExchangeError) Error() string {
	if e == nil {
		return ""
	}
	parts := []string{fmt.Sprintf("token exchange status=%d", e.StatusCode)}
	if strings.TrimSpace(e.ProviderError) != "" {
		parts = append(parts, "error="+strings.TrimSpace(e.ProviderError))
	}
	if strings.TrimSpace(e.ProviderDescription) != "" {
		parts = append(parts, "error_description="+strings.TrimSpace(e.ProviderDescription))
	}
	return strings.Join(parts, " ")
}

// OxSciOAuthStart 启动 OxSci OAuth 登录流程。
// GET /api/v1/auth/oauth/oxsci/start?redirect=/dashboard
func (h *AuthHandler) OxSciOAuthStart(c *gin.Context) {
	cfg, err := h.getOxSciOAuthConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}

	state, err := oauth.GenerateState()
	if err != nil {
		response.ErrorFrom(c, infraerrors.InternalServer("OAUTH_STATE_GEN_FAILED", "failed to generate oauth state").WithCause(err))
		return
	}

	redirectTo := sanitizeOxSciFrontendRedirectPath(c.Query("redirect"))
	if redirectTo == "" {
		redirectTo = oxsciOAuthDefaultRedirectTo
	}

	secureCookie := isOxSciRequestHTTPS(c)
	setOxSciCookie(c, oxsciOAuthStateCookieName, encodeOxSciCookieValue(state), oxsciOAuthCookieMaxAgeSec, secureCookie)
	setOxSciCookie(c, oxsciOAuthRedirectCookie, encodeOxSciCookieValue(redirectTo), oxsciOAuthCookieMaxAgeSec, secureCookie)

	codeChallenge := ""
	if cfg.UsePKCE {
		verifier, err := oauth.GenerateCodeVerifier()
		if err != nil {
			response.ErrorFrom(c, infraerrors.InternalServer("OAUTH_PKCE_GEN_FAILED", "failed to generate pkce verifier").WithCause(err))
			return
		}
		codeChallenge = oauth.GenerateCodeChallenge(verifier)
		setOxSciCookie(c, oxsciOAuthVerifierCookie, encodeOxSciCookieValue(verifier), oxsciOAuthCookieMaxAgeSec, secureCookie)
	}

	redirectURI := strings.TrimSpace(cfg.RedirectURL)
	if redirectURI == "" {
		response.ErrorFrom(c, infraerrors.InternalServer("OAUTH_CONFIG_INVALID", "oauth redirect url not configured"))
		return
	}

	authURL, err := buildOxSciAuthorizeURL(cfg, state, codeChallenge, redirectURI)
	if err != nil {
		response.ErrorFrom(c, infraerrors.InternalServer("OAUTH_BUILD_URL_FAILED", "failed to build oauth authorization url").WithCause(err))
		return
	}

	c.Redirect(http.StatusFound, authURL)
}

// OxSciOAuthCallback 处理 OAuth 回调：创建/登录用户，然后重定向到前端。
// GET /api/v1/auth/oauth/oxsci/callback?code=...&state=...
func (h *AuthHandler) OxSciOAuthCallback(c *gin.Context) {
	cfg, cfgErr := h.getOxSciOAuthConfig(c.Request.Context())
	if cfgErr != nil {
		response.ErrorFrom(c, cfgErr)
		return
	}

	frontendCallback := strings.TrimSpace(cfg.FrontendRedirectURL)
	if frontendCallback == "" {
		frontendCallback = oxsciOAuthDefaultFrontendCB
	}

	if providerErr := strings.TrimSpace(c.Query("error")); providerErr != "" {
		redirectOxSciOAuthError(c, frontendCallback, "provider_error", providerErr, c.Query("error_description"))
		return
	}

	code := strings.TrimSpace(c.Query("code"))
	state := strings.TrimSpace(c.Query("state"))
	if code == "" || state == "" {
		redirectOxSciOAuthError(c, frontendCallback, "missing_params", "missing code/state", "")
		return
	}

	secureCookie := isOxSciRequestHTTPS(c)
	defer func() {
		clearOxSciCookie(c, oxsciOAuthStateCookieName, secureCookie)
		clearOxSciCookie(c, oxsciOAuthVerifierCookie, secureCookie)
		clearOxSciCookie(c, oxsciOAuthRedirectCookie, secureCookie)
	}()

	expectedState, err := readOxSciCookieDecoded(c, oxsciOAuthStateCookieName)
	if err != nil || expectedState == "" || state != expectedState {
		redirectOxSciOAuthError(c, frontendCallback, "invalid_state", "invalid oauth state", "")
		return
	}

	redirectTo, _ := readOxSciCookieDecoded(c, oxsciOAuthRedirectCookie)
	redirectTo = sanitizeOxSciFrontendRedirectPath(redirectTo)
	if redirectTo == "" {
		redirectTo = oxsciOAuthDefaultRedirectTo
	}

	codeVerifier := ""
	if cfg.UsePKCE {
		codeVerifier, _ = readOxSciCookieDecoded(c, oxsciOAuthVerifierCookie)
		if codeVerifier == "" {
			redirectOxSciOAuthError(c, frontendCallback, "missing_verifier", "missing pkce verifier", "")
			return
		}
	}

	redirectURI := strings.TrimSpace(cfg.RedirectURL)
	if redirectURI == "" {
		redirectOxSciOAuthError(c, frontendCallback, "config_error", "oauth redirect url not configured", "")
		return
	}

	tokenResp, err := oxsciExchangeCode(c.Request.Context(), cfg, code, redirectURI, codeVerifier)
	if err != nil {
		description := ""
		var exchangeErr *oxsciTokenExchangeError
		if errors.As(err, &exchangeErr) && exchangeErr != nil {
			log.Printf(
				"[OxSci OAuth] token exchange failed: status=%d provider_error=%q provider_description=%q body=%s",
				exchangeErr.StatusCode,
				exchangeErr.ProviderError,
				exchangeErr.ProviderDescription,
				truncateOxSciLogValue(exchangeErr.Body, 2048),
			)
			description = exchangeErr.Error()
		} else {
			log.Printf("[OxSci OAuth] token exchange failed: %v", err)
			description = err.Error()
		}
		redirectOxSciOAuthError(c, frontendCallback, "token_exchange_failed", "failed to exchange oauth code", singleLineOxSci(description))
		return
	}

	email, username, subject, err := oxsciFetchUserInfo(c.Request.Context(), cfg, tokenResp)
	if err != nil {
		log.Printf("[OxSci OAuth] userinfo fetch failed: %v", err)
		redirectOxSciOAuthError(c, frontendCallback, "userinfo_failed", "failed to fetch user info", "")
		return
	}

	// OxSci OAuth: 直接使用 OxSci 返回的真实邮箱进行账号绑定
	// 不使用合成邮箱，因为我们信任 OxSci 的用户身份
	// 使用 Trusted 版本，跳过注册开关检查，允许自动创建用户
	_ = subject // subject 仅用于日志记录
	log.Printf("[OxSci OAuth] User authenticated: email=%s, username=%s, subject=%s", email, username, subject)

	jwtToken, _, err := h.authService.LoginOrRegisterOAuthTrusted(c.Request.Context(), email, username)
	if err != nil {
		redirectOxSciOAuthError(c, frontendCallback, "login_failed", infraerrors.Reason(err), infraerrors.Message(err))
		return
	}

	fragment := url.Values{}
	fragment.Set("access_token", jwtToken)
	fragment.Set("token_type", "Bearer")
	fragment.Set("redirect", redirectTo)
	redirectOxSciWithFragment(c, frontendCallback, fragment)
}

func (h *AuthHandler) getOxSciOAuthConfig(ctx context.Context) (config.OxSciOAuthConfig, error) {
	// OxSci OAuth 配置只从配置文件读取，不支持动态配置
	if h == nil || h.cfg == nil {
		return config.OxSciOAuthConfig{}, infraerrors.ServiceUnavailable("CONFIG_NOT_READY", "config not loaded")
	}
	if !h.cfg.OxSci.Enabled {
		return config.OxSciOAuthConfig{}, infraerrors.NotFound("OAUTH_DISABLED", "oxsci oauth login is disabled")
	}
	return h.cfg.OxSci, nil
}

func oxsciExchangeCode(
	ctx context.Context,
	cfg config.OxSciOAuthConfig,
	code string,
	redirectURI string,
	codeVerifier string,
) (*oxsciTokenResponse, error) {
	client := req.C().SetTimeout(30 * time.Second)

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", cfg.ClientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	if cfg.UsePKCE && codeVerifier != "" {
		form.Set("code_verifier", codeVerifier)
	}

	r := client.R().
		SetContext(ctx).
		SetHeader("Accept", "application/json")

	// OxSci OAuth 使用 client_secret_post 方法
	if cfg.ClientSecret != "" {
		form.Set("client_secret", cfg.ClientSecret)
	}

	resp, err := r.SetFormDataFromValues(form).Post(cfg.TokenURL)
	if err != nil {
		return nil, fmt.Errorf("request token: %w", err)
	}
	body := strings.TrimSpace(resp.String())
	if !resp.IsSuccessState() {
		providerErr, providerDesc := parseOxSciOAuthProviderError(body)
		return nil, &oxsciTokenExchangeError{
			StatusCode:          resp.StatusCode,
			ProviderError:       providerErr,
			ProviderDescription: providerDesc,
			Body:                body,
		}
	}

	tokenResp, ok := parseOxSciTokenResponse(body)
	if !ok || strings.TrimSpace(tokenResp.AccessToken) == "" {
		return nil, &oxsciTokenExchangeError{
			StatusCode: resp.StatusCode,
			Body:       body,
		}
	}
	if strings.TrimSpace(tokenResp.TokenType) == "" {
		tokenResp.TokenType = "Bearer"
	}
	return tokenResp, nil
}

func oxsciFetchUserInfo(
	ctx context.Context,
	cfg config.OxSciOAuthConfig,
	token *oxsciTokenResponse,
) (email string, username string, subject string, err error) {
	client := req.C().SetTimeout(30 * time.Second)
	authorization, err := buildOxSciBearerAuthorization(token.TokenType, token.AccessToken)
	if err != nil {
		return "", "", "", fmt.Errorf("invalid token for userinfo request: %w", err)
	}

	resp, err := client.R().
		SetContext(ctx).
		SetHeader("Accept", "application/json").
		SetHeader("Authorization", authorization).
		Get(cfg.UserInfoURL)
	if err != nil {
		return "", "", "", fmt.Errorf("request userinfo: %w", err)
	}
	if !resp.IsSuccessState() {
		return "", "", "", fmt.Errorf("userinfo status=%d", resp.StatusCode)
	}

	return oxsciParseUserInfo(resp.String())
}

func oxsciParseUserInfo(body string) (email string, username string, subject string, err error) {
	// OxSci userinfo 返回标准 OIDC 格式
	email = firstOxSciNonEmpty(
		getOxSciGJSON(body, "email"),
		getOxSciGJSON(body, "data.email"),
	)
	username = firstOxSciNonEmpty(
		getOxSciGJSON(body, "preferred_username"),
		getOxSciGJSON(body, "name"),
		getOxSciGJSON(body, "full_name"),
		getOxSciGJSON(body, "data.name"),
	)
	subject = firstOxSciNonEmpty(
		getOxSciGJSON(body, "sub"),
		getOxSciGJSON(body, "id"),
		getOxSciGJSON(body, "user_id"),
		getOxSciGJSON(body, "data.id"),
	)

	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "", "", "", errors.New("userinfo missing id field")
	}
	if !isSafeOxSciSubject(subject) {
		return "", "", "", errors.New("userinfo returned invalid id field")
	}

	email = strings.TrimSpace(email)
	if email == "" {
		// 如果没有邮箱，使用合成邮箱
		email = oxsciSyntheticEmail(subject)
	}

	username = strings.TrimSpace(username)
	if username == "" {
		username = "oxsci_" + subject
	}

	return email, username, subject, nil
}

func buildOxSciAuthorizeURL(cfg config.OxSciOAuthConfig, state string, codeChallenge string, redirectURI string) (string, error) {
	u, err := url.Parse(cfg.AuthorizeURL)
	if err != nil {
		return "", fmt.Errorf("parse authorize_url: %w", err)
	}

	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", redirectURI)
	if strings.TrimSpace(cfg.Scopes) != "" {
		q.Set("scope", cfg.Scopes)
	}
	q.Set("state", state)
	if cfg.UsePKCE && codeChallenge != "" {
		q.Set("code_challenge", codeChallenge)
		q.Set("code_challenge_method", "S256")
	}

	u.RawQuery = q.Encode()
	return u.String(), nil
}

func redirectOxSciOAuthError(c *gin.Context, frontendCallback string, code string, message string, description string) {
	fragment := url.Values{}
	fragment.Set("error", truncateOxSciFragmentValue(code))
	if strings.TrimSpace(message) != "" {
		fragment.Set("error_message", truncateOxSciFragmentValue(message))
	}
	if strings.TrimSpace(description) != "" {
		fragment.Set("error_description", truncateOxSciFragmentValue(description))
	}
	redirectOxSciWithFragment(c, frontendCallback, fragment)
}

func redirectOxSciWithFragment(c *gin.Context, frontendCallback string, fragment url.Values) {
	u, err := url.Parse(frontendCallback)
	if err != nil {
		c.Redirect(http.StatusFound, oxsciOAuthDefaultRedirectTo)
		return
	}
	if u.Scheme != "" && !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		c.Redirect(http.StatusFound, oxsciOAuthDefaultRedirectTo)
		return
	}
	u.Fragment = fragment.Encode()
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Redirect(http.StatusFound, u.String())
}

func firstOxSciNonEmpty(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func parseOxSciOAuthProviderError(body string) (providerErr string, providerDesc string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return "", ""
	}

	providerErr = firstOxSciNonEmpty(
		getOxSciGJSON(body, "error"),
		getOxSciGJSON(body, "code"),
		getOxSciGJSON(body, "error.code"),
	)
	providerDesc = firstOxSciNonEmpty(
		getOxSciGJSON(body, "error_description"),
		getOxSciGJSON(body, "error.message"),
		getOxSciGJSON(body, "message"),
		getOxSciGJSON(body, "detail"),
	)

	if providerErr != "" || providerDesc != "" {
		return providerErr, providerDesc
	}

	values, err := url.ParseQuery(body)
	if err != nil {
		return "", ""
	}
	providerErr = firstOxSciNonEmpty(values.Get("error"), values.Get("code"))
	providerDesc = firstOxSciNonEmpty(values.Get("error_description"), values.Get("error_message"), values.Get("message"))
	return providerErr, providerDesc
}

func parseOxSciTokenResponse(body string) (*oxsciTokenResponse, bool) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, false
	}

	accessToken := strings.TrimSpace(getOxSciGJSON(body, "access_token"))
	if accessToken != "" {
		tokenType := strings.TrimSpace(getOxSciGJSON(body, "token_type"))
		refreshToken := strings.TrimSpace(getOxSciGJSON(body, "refresh_token"))
		scope := strings.TrimSpace(getOxSciGJSON(body, "scope"))
		expiresIn := gjson.Get(body, "expires_in").Int()
		return &oxsciTokenResponse{
			AccessToken:  accessToken,
			TokenType:    tokenType,
			ExpiresIn:    expiresIn,
			RefreshToken: refreshToken,
			Scope:        scope,
		}, true
	}

	return nil, false
}

func getOxSciGJSON(body string, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	res := gjson.Get(body, path)
	if !res.Exists() {
		return ""
	}
	return res.String()
}

func truncateOxSciLogValue(value string, maxLen int) string {
	value = strings.TrimSpace(value)
	if value == "" || maxLen <= 0 {
		return ""
	}
	if len(value) <= maxLen {
		return value
	}
	value = value[:maxLen]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func singleLineOxSci(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return strings.Join(strings.Fields(value), " ")
}

func sanitizeOxSciFrontendRedirectPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if len(path) > oxsciOAuthMaxRedirectLen {
		return ""
	}
	// 只允许同源相对路径（避免开放重定向）。
	if !strings.HasPrefix(path, "/") {
		return ""
	}
	if strings.HasPrefix(path, "//") {
		return ""
	}
	if strings.Contains(path, "://") {
		return ""
	}
	if strings.ContainsAny(path, "\r\n") {
		return ""
	}
	return path
}

func isOxSciRequestHTTPS(c *gin.Context) bool {
	if c.Request.TLS != nil {
		return true
	}
	proto := strings.ToLower(strings.TrimSpace(c.GetHeader("X-Forwarded-Proto")))
	return proto == "https"
}

func encodeOxSciCookieValue(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeOxSciCookieValue(value string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func readOxSciCookieDecoded(c *gin.Context, name string) (string, error) {
	ck, err := c.Request.Cookie(name)
	if err != nil {
		return "", err
	}
	return decodeOxSciCookieValue(ck.Value)
}

func setOxSciCookie(c *gin.Context, name string, value string, maxAgeSec int, secure bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     oxsciOAuthCookiePath,
		MaxAge:   maxAgeSec,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearOxSciCookie(c *gin.Context, name string, secure bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     oxsciOAuthCookiePath,
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func truncateOxSciFragmentValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if len(value) > oxsciOAuthMaxFragmentValueLen {
		value = value[:oxsciOAuthMaxFragmentValueLen]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

func buildOxSciBearerAuthorization(tokenType, accessToken string) (string, error) {
	tokenType = strings.TrimSpace(tokenType)
	if tokenType == "" {
		tokenType = "Bearer"
	}
	if !strings.EqualFold(tokenType, "Bearer") {
		return "", fmt.Errorf("unsupported token_type: %s", tokenType)
	}

	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return "", errors.New("missing access_token")
	}
	if strings.ContainsAny(accessToken, " \t\r\n") {
		return "", errors.New("access_token contains whitespace")
	}
	return "Bearer " + accessToken, nil
}

func isSafeOxSciSubject(subject string) bool {
	subject = strings.TrimSpace(subject)
	if subject == "" || len(subject) > oxsciOAuthMaxSubjectLen {
		return false
	}
	for _, r := range subject {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

func oxsciSyntheticEmail(subject string) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return ""
	}
	return "oxsci-" + subject + service.OxSciConnectSyntheticEmailDomain
}
