package framework

import (
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/ibm-cloud-security/policy-enforcer-mixer-adapter/adapter/authserver"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
)

const (
	defaultUsername = "testuser"
	defaultPassword = "password"
	clientID        = "APPID_CLIENT_ID"
	clientSecret    = "APPID_CLIENT_SECRET"
	oauthServerUrl  = "APPID_OAUTH_SERVER_URL"
)

const (
	tokenEndpoint          = "/token"
	publicKeysEndpoint     = "/publickeys"
	discoveryEndpoint      = "/.well-known/openid-configuration"
	appIDCDAuthEndpoint    = "/cloud_directory/auth"
	callbackEndpoint       = "/oidc/callback"
	applicationFormEncoded = "application/x-www-form-urlencoded"
	contentType            = "Content-Type"
	setCookie              = "set-cookie"
	appIDStateID           = "#SAML_State"
	widgetURLID            = "#widgetUrl"
)

// AppIDManager models the authorization server
type AppIDManager struct {
	ClientID       string
	ClientSecret   string
	OAuthServerURL string
	Tokens         *authserver.TokenResponse
	client         *http.Client
}

// NewAppIDManager creates a new manager from environment variables
func NewAppIDManager() *AppIDManager {
	return &AppIDManager{
		os.Getenv(clientID),
		os.Getenv(clientSecret),
		os.Getenv(oauthServerUrl),
		nil,
		&http.Client{
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// OK validates an AppIDManager
func (m *AppIDManager) OK() bool {
	return m.ClientID != "" && m.ClientSecret != "" && m.OAuthServerURL != "" && m.client != nil
}

// TokenURL returns the token URL of the AppIDManager
func (m *AppIDManager) TokenURL() string {
	return m.OAuthServerURL + tokenEndpoint
}

// PublicKeysURL returns the public keys URL of the AppIDManager
func (m *AppIDManager) PublicKeysURL() string {
	return m.OAuthServerURL + publicKeysEndpoint
}

// DiscoveryURL returns the well known URL of the AppIDManager
func (m *AppIDManager) DiscoveryURL() string {
	return m.OAuthServerURL + discoveryEndpoint
}

// LoginToCloudDirectory logs in through an App ID cloud directory login widget returning the result from the login page
func (m *AppIDManager) LoginToCloudDirectory(t *testing.T, root string, path string, output interface{}) error {

	state, widgetURL := m.initialRequestToFrontend(t, root+path)

	adapterCallbackURL, err := m.postCredentialsFromWidget(root+path+callbackEndpoint, state, widgetURL)
	require.NoError(t, err)

	oidcCookie, adapterOriginalURL, err := m.forwardRedirectToAdapter(adapterCallbackURL)
	require.NoError(t, err)

	return m.sendAuthenticatedRequest(adapterOriginalURL, oidcCookie, output)
}

// ROP issues an ROP token flow again the authorization server instance
func (m *AppIDManager) ROP(username string, password string) error {
	form := url.Values{
		"client_id":  {m.ClientID},
		"grant_type": {"password"},
		"username":   {username},
		"password":   {password},
	}

	req, err := http.NewRequest("POST", m.TokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		zap.L().Error("Could not serialize HTTP request", zap.Error(err))
		return err
	}

	req.SetBasicAuth(m.ClientID, m.ClientSecret)
	req.Header.Set(contentType, applicationFormEncoded)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}

	if res.StatusCode != http.StatusOK {
		return fmt.Errorf(res.Status)
	}

	var tokenResponse authserver.TokenResponse
	if err := json.NewDecoder(res.Body).Decode(&tokenResponse); err != nil {
		str, _ := ioutil.ReadAll(res.Body)
		return fmt.Errorf("body %s | %s", string(str), err)
	}

	m.Tokens = &tokenResponse
	return nil
}

///
/// App ID utility request functions to handle OIDC flow without UI
/// Redirect cannot be used as cookies will not be automatically set
///

func (m *AppIDManager) initialRequestToFrontend(t *testing.T, path string) (string, string) {

	res, err := http.DefaultClient.Get(path) // Use default to allow redirect
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, res.StatusCode)
	defer res.Body.Close()

	// Parse login page
	doc, err := goquery.NewDocumentFromReader(res.Body)
	state, ok := doc.Find(appIDStateID).Attr("value")
	widgetUrl, okW := doc.Find(widgetURLID).Attr("value")
	require.True(t, ok)
	require.True(t, okW)
	return state, widgetUrl
}

func (m *AppIDManager) postCredentialsFromWidget(redirectUri string, state string, widgetURL string) (*url.URL, error) {
	form := url.Values{
		"email":       {defaultUsername},
		"password":    {defaultPassword},
		"redirectUri": {redirectUri},
		"clientId":    {m.ClientID},
		"state":       {state},
	}

	req, err := http.NewRequest("POST", m.OAuthServerURL+appIDCDAuthEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}

	req.Header.Set(contentType, applicationFormEncoded)

	res, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}

	defer res.Body.Close()
	if res.StatusCode != http.StatusFound {
		str, _ := ioutil.ReadAll(res.Body)
		return nil, fmt.Errorf("received unexpected response : %s | %s", res.Status, string(str))
	}

	return res.Location()
}

func (m *AppIDManager) forwardRedirectToAdapter(url *url.URL) (*http.Cookie, *url.URL, error) {
	req, err := http.NewRequest("GET", url.String(), nil)
	req.URL = url

	if err != nil {
		return nil, nil, err
	}

	res, err := m.client.Do(req)
	if err != nil {
		return nil, nil, err
	}

	defer res.Body.Close()
	if res.StatusCode != http.StatusFound {
		str, _ := ioutil.ReadAll(res.Body)
		return nil, nil, fmt.Errorf("received unexpected response : %s | %s", res.Status, string(str))
	}

	url2, err := res.Location()
	rawCookies := res.Header.Get(setCookie)
	header := http.Header{}
	header.Add("Cookie", rawCookies)
	temp := http.Request{Header: header}
	var oidcCookie *http.Cookie
	for _, cookie := range temp.Cookies() {
		if strings.Contains(cookie.Name, "oidc-cookie") {
			oidcCookie = cookie
		}
	}
	if oidcCookie == nil {
		return nil, nil, fmt.Errorf("oidc cookie not provided")
	}
	return oidcCookie, url2, err
}

func (m *AppIDManager) sendAuthenticatedRequest(url *url.URL, cookie *http.Cookie, output interface{}) error {
	req, err := http.NewRequest("GET", url.String(), nil)
	req.URL = url
	req.AddCookie(cookie)
	if err != nil {
		return err
	}

	res, err := m.client.Do(req)
	if err != nil {
		return err
	}

	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		str, _ := ioutil.ReadAll(res.Body)
		return fmt.Errorf("received unexpected response : %s | %s", res.Status, string(str))
	}

	if err := json.NewDecoder(res.Body).Decode(&output); err != nil {
		str, _ := ioutil.ReadAll(res.Body)
		return fmt.Errorf("body %s | %s", string(str), err)
	}

	return nil
}