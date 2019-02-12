/*
Copyright 2017-2019 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auth

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"

	phttp "github.com/coreos/go-oidc/http"
	"github.com/coreos/go-oidc/jose"
	"github.com/coreos/go-oidc/oauth2"
	"github.com/coreos/go-oidc/oidc"
	"github.com/gravitational/trace"
)

func (s *AuthServer) getOIDCClient(conn services.OIDCConnector) (*oidc.Client, error) {
	s.lock.Lock()
	defer s.lock.Unlock()

	config := oidc.ClientConfig{
		RedirectURL: conn.GetRedirectURL(),
		Credentials: oidc.ClientCredentials{
			ID:     conn.GetClientID(),
			Secret: conn.GetClientSecret(),
		},
		// open id notifies provider that we are using OIDC scopes
		Scope: utils.Deduplicate(append([]string{"openid", "email"}, conn.GetScope()...)),
	}

	clientPack, ok := s.oidcClients[conn.GetName()]
	if ok && oidcConfigsEqual(clientPack.config, config) {
		return clientPack.client, nil
	}
	delete(s.oidcClients, conn.GetName())

	client, err := oidc.NewClient(config)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	client.SyncProviderConfig(conn.GetIssuerURL())

	s.oidcClients[conn.GetName()] = &oidcClient{client: client, config: config}

	return client, nil
}

func (s *AuthServer) UpsertOIDCConnector(connector services.OIDCConnector) error {
	return s.Identity.UpsertOIDCConnector(connector)
}

func (s *AuthServer) DeleteOIDCConnector(connectorName string) error {
	return s.Identity.DeleteOIDCConnector(connectorName)
}

func (s *AuthServer) CreateOIDCAuthRequest(req services.OIDCAuthRequest) (*services.OIDCAuthRequest, error) {
	connector, err := s.Identity.GetOIDCConnector(req.ConnectorID, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	oidcClient, err := s.getOIDCClient(connector)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	oauthClient, err := oidcClient.OAuthClient()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	stateToken, err := utils.CryptoRandomHex(TokenLenBytes)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	req.StateToken = stateToken
	// online is OIDC online scope, "select_account" forces user to always select account
	req.RedirectURL = oauthClient.AuthCodeURL(req.StateToken, "online", "select_account")

	// if the connector has an Authentication Context Class Reference (ACR) value set,
	// update redirect url and add it as a query value.
	acrValue := connector.GetACR()
	if acrValue != "" {
		u, err := url.Parse(req.RedirectURL)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		q := u.Query()
		q.Set("acr_values", acrValue)
		u.RawQuery = q.Encode()
		req.RedirectURL = u.String()
	}

	log.Debugf("OIDC redirect URL: %v.", req.RedirectURL)

	err = s.Identity.CreateOIDCAuthRequest(req, defaults.OIDCAuthRequestTTL)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &req, nil
}

// ValidateOIDCAuthCallback is called by the proxy to check OIDC query parameters
// returned by OIDC Provider, if everything checks out, auth server
// will respond with OIDCAuthResponse, otherwise it will return error
func (a *AuthServer) ValidateOIDCAuthCallback(q url.Values) (*OIDCAuthResponse, error) {
	re, err := a.validateOIDCAuthCallback(q)
	if err != nil {
		a.EmitAuditEvent(events.UserLoginEvent, events.EventFields{
			events.LoginMethod:        events.LoginMethodOIDC,
			events.AuthAttemptSuccess: false,
			events.AuthAttemptErr:     err.Error(),
		})
	} else {
		a.EmitAuditEvent(events.UserLoginEvent, events.EventFields{
			events.EventUser:          re.Username,
			events.AuthAttemptSuccess: true,
			events.LoginMethod:        events.LoginMethodOIDC,
		})
	}
	return re, err
}

func (a *AuthServer) validateOIDCAuthCallback(q url.Values) (*OIDCAuthResponse, error) {
	if error := q.Get("error"); error != "" {
		return nil, trace.OAuth2(oauth2.ErrorInvalidRequest, error, q)
	}

	code := q.Get("code")
	if code == "" {
		return nil, trace.OAuth2(
			oauth2.ErrorInvalidRequest, "code query param must be set", q)
	}

	stateToken := q.Get("state")
	if stateToken == "" {
		return nil, trace.OAuth2(
			oauth2.ErrorInvalidRequest, "missing state query param", q)
	}

	clusterName, err := a.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	req, err := a.Identity.GetOIDCAuthRequest(stateToken)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	connector, err := a.Identity.GetOIDCConnector(req.ConnectorID, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	oidcClient, err := a.getOIDCClient(connector)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// extract claims from both the id token and the userinfo endpoint and merge them
	claims, err := a.getClaims(oidcClient, connector.GetIssuerURL(), connector.GetScope(), code)
	if err != nil {
		return nil, trace.OAuth2(
			oauth2.ErrorUnsupportedResponseType, "unable to construct claims", q)
	}

	log.Debugf("OIDC claims: %v.", claims)

	// if we are sending acr values, make sure we also validate them
	acrValue := connector.GetACR()
	if acrValue != "" {
		err := a.validateACRValues(acrValue, connector.GetProvider(), claims)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		log.Debugf("OIDC ACR values %q successfully validated.", acrValue)
	}

	ident, err := oidc.IdentityFromClaims(claims)
	if err != nil {
		return nil, trace.OAuth2(
			oauth2.ErrorUnsupportedResponseType, "unable to convert claims to identity", q)
	}
	log.Debugf("OIDC user %q expires at: %v.", ident.Email, ident.ExpiresAt)

	response := &OIDCAuthResponse{
		Identity: services.ExternalIdentity{ConnectorID: connector.GetName(), Username: ident.Email},
		Req:      *req,
	}

	log.Debugf("Applying %v OIDC claims to roles mappings.", len(connector.GetClaimsToRoles()))
	if len(connector.GetClaimsToRoles()) != 0 {
		if err := a.createOIDCUser(connector, ident, claims); err != nil {
			return nil, trace.Wrap(err)
		}
	}

	if !req.CheckUser {
		return response, nil
	}

	user, err := a.Identity.GetUserByOIDCIdentity(services.ExternalIdentity{
		ConnectorID: req.ConnectorID, Username: ident.Email})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	response.Username = user.GetName()

	var roles services.RoleSet
	roles, err = services.FetchRoles(user.GetRoles(), a.Access, user.GetTraits())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sessionTTL := roles.AdjustSessionTTL(utils.ToTTL(a.clock, ident.ExpiresAt))
	bearerTokenTTL := utils.MinTTL(BearerTokenTTL, sessionTTL)

	if req.CreateWebSession {
		sess, err := a.NewWebSession(user.GetName())
		if err != nil {
			return nil, trace.Wrap(err)
		}
		// session will expire based on identity TTL and allowed session TTL
		sess.SetExpiryTime(a.clock.Now().UTC().Add(sessionTTL))
		// bearer token will expire based on the expected session renewal
		sess.SetBearerTokenExpiryTime(a.clock.Now().UTC().Add(bearerTokenTTL))
		if err := a.UpsertWebSession(user.GetName(), sess); err != nil {
			return nil, trace.Wrap(err)
		}
		response.Session = sess
	}

	if len(req.PublicKey) != 0 {
		certTTL := utils.MinTTL(utils.ToTTL(a.clock, ident.ExpiresAt), req.CertTTL)
		certs, err := a.generateUserCert(certRequest{
			user:          user,
			roles:         roles,
			ttl:           certTTL,
			publicKey:     req.PublicKey,
			compatibility: req.Compatibility,
		})
		if err != nil {
			return nil, trace.Wrap(err)
		}
		response.Cert = certs.ssh
		response.TLSCert = certs.tls

		// Return the host CA for this cluster only.
		authority, err := a.GetCertAuthority(services.CertAuthID{
			Type:       services.HostCA,
			DomainName: clusterName.GetClusterName(),
		}, false)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		response.HostSigners = append(response.HostSigners, authority)
	}
	return response, nil
}

// OIDCAuthResponse is returned when auth server validated callback parameters
// returned from OIDC provider
type OIDCAuthResponse struct {
	// Username is authenticated teleport username
	Username string `json:"username"`
	// Identity contains validated OIDC identity
	Identity services.ExternalIdentity `json:"identity"`
	// Web session will be generated by auth server if requested in OIDCAuthRequest
	Session services.WebSession `json:"session,omitempty"`
	// Cert will be generated by certificate authority
	Cert []byte `json:"cert,omitempty"`
	// TLSCert is PEM encoded TLS certificate
	TLSCert []byte `json:"tls_cert,omitempty"`
	// Req is original oidc auth request
	Req services.OIDCAuthRequest `json:"req"`
	// HostSigners is a list of signing host public keys
	// trusted by proxy, used in console login
	HostSigners []services.CertAuthority `json:"host_signers"`
}

// buildOIDCRoles takes a connector and claims and returns a slice of roles.
func (a *AuthServer) buildOIDCRoles(connector services.OIDCConnector, claims jose.Claims) ([]string, error) {
	roles := connector.MapClaims(claims)
	if len(roles) == 0 {
		return nil, trace.AccessDenied("unable to map claims to role for connector: %v", connector.GetName())
	}

	return roles, nil
}

// claimsToTraitMap extracts all string claims and creates a map of traits
// that can be used to populate role variables.
func claimsToTraitMap(claims jose.Claims) map[string][]string {
	traits := make(map[string][]string)

	for claimName := range claims {
		claimValue, ok, _ := claims.StringClaim(claimName)
		if ok {
			traits[claimName] = []string{claimValue}
		}
		claimValues, ok, _ := claims.StringsClaim(claimName)
		if ok {
			traits[claimName] = claimValues
		}
	}

	return traits
}

func (a *AuthServer) createOIDCUser(connector services.OIDCConnector, ident *oidc.Identity, claims jose.Claims) error {
	roles, err := a.buildOIDCRoles(connector, claims)
	if err != nil {
		return trace.Wrap(err)
	}

	traits := claimsToTraitMap(claims)

	log.Debugf("Generating dynamic OIDC identity %v/%v with roles: %v.", connector.GetName(), ident.Email, roles)
	user, err := services.GetUserMarshaler().GenerateUser(&services.UserV2{
		Kind:    services.KindUser,
		Version: services.V2,
		Metadata: services.Metadata{
			Name:      ident.Email,
			Namespace: defaults.Namespace,
		},
		Spec: services.UserSpecV2{
			Roles:   roles,
			Traits:  traits,
			Expires: ident.ExpiresAt,
			OIDCIdentities: []services.ExternalIdentity{
				{
					ConnectorID: connector.GetName(),
					Username:    ident.Email,
				},
			},
			CreatedBy: services.CreatedBy{
				User: services.UserRef{Name: "system"},
				Time: time.Now().UTC(),
				Connector: &services.ConnectorRef{
					Type:     teleport.ConnectorOIDC,
					ID:       connector.GetName(),
					Identity: ident.Email,
				},
			},
		},
	})
	if err != nil {
		return trace.Wrap(err)
	}

	// Get the user to check if it already exists or not.
	existingUser, err := a.GetUser(ident.Email)
	if err != nil {
		if !trace.IsNotFound(err) {
			return trace.Wrap(err)
		}
	}

	// Overwrite exisiting user if it was created from an external identity provider.
	if existingUser != nil {
		connectorRef := existingUser.GetCreatedBy().Connector

		// If the exisiting user is a local user, fail and advise how to fix the problem.
		if connectorRef == nil {
			return trace.AlreadyExists("local user with name '%v' already exists. Either change "+
				"email in OIDC identity or remove local user and try again.", existingUser.GetName())
		}

		log.Debugf("Overwriting exisiting user '%v' created with %v connector %v.",
			existingUser.GetName(), connectorRef.Type, connectorRef.ID)
	}

	// Upsert the new user creating or updating whatever is in the database.
	err = a.UpsertUser(user)
	if err != nil {
		return trace.Wrap(err)
	}

	return nil
}

// claimsFromIDToken extracts claims from the ID token.
func claimsFromIDToken(oidcClient *oidc.Client, idToken string) (jose.Claims, error) {
	jwt, err := jose.ParseJWT(idToken)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	err = oidcClient.VerifyJWT(jwt)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	log.Debugf("Extracting OIDC claims from ID token.")

	claims, err := jwt.Claims()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return claims, nil
}

// claimsFromUserInfo finds the UserInfo endpoint from the provider config and then extracts claims from it.
//
// Note: We don't request signed JWT responses for UserInfo, instead we force the provider config and
// the issuer to be HTTPS and leave integrity and confidentiality to TLS. Authenticity is taken care of
// during the token exchange.
func claimsFromUserInfo(oidcClient *oidc.Client, issuerURL string, accessToken string) (jose.Claims, error) {
	err := isHTTPS(issuerURL)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	oac, err := oidcClient.OAuthClient()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	hc := oac.HttpClient()

	// go get the provider config so we can find out where the UserInfo endpoint
	// is. if the provider doesn't offer a UserInfo endpoint return not found.
	pc, err := oidc.FetchProviderConfig(oac.HttpClient(), issuerURL)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if pc.UserInfoEndpoint == nil {
		return nil, trace.NotFound("UserInfo endpoint not found")
	}

	endpoint := pc.UserInfoEndpoint.String()
	err = isHTTPS(endpoint)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	log.Debugf("Fetching OIDC claims from UserInfo endpoint: %q.", endpoint)

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))

	resp, err := hc.Do(req)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, trace.AccessDenied("bad status code: %v", resp.StatusCode)
	}

	var claims jose.Claims
	err = json.NewDecoder(resp.Body).Decode(&claims)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return claims, nil
}

func (a *AuthServer) claimsFromGSuite(oidcClient *oidc.Client, issuerURL string, userEmail string, accessToken string) (jose.Claims, error) {
	client, err := a.newGsuiteClient(oidcClient, issuerURL, userEmail, accessToken)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return client.fetchGroups()
}

func (a *AuthServer) newGsuiteClient(oidcClient *oidc.Client, issuerURL string, userEmail string, accessToken string) (*gsuiteClient, error) {
	err := isHTTPS(issuerURL)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	oac, err := oidcClient.OAuthClient()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	u, err := url.Parse(teleport.GSuiteGroupsEndpoint)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	return &gsuiteClient{
		Client:      oac.HttpClient(),
		url:         *u,
		userEmail:   userEmail,
		accessToken: accessToken,
		auditLog:    a,
	}, nil
}

type gsuiteClient struct {
	phttp.Client
	url         url.URL
	userEmail   string
	accessToken string
	auditLog    events.IAuditLog
}

// fetchGroups fetches GSuite groups a user belongs to and returns
// "groups" claim with
func (g *gsuiteClient) fetchGroups() (jose.Claims, error) {
	count := 0
	var groups []string
	var nextPageToken string
collect:
	for {
		if count > MaxPages {
			warningMessage := "Truncating list of teams used to populate claims: " +
				"hit maximum number pages that can be fetched from GSuite."

			// Print warning to Teleport logs as well as the Audit Log.
			log.Warnf(warningMessage)
			g.auditLog.EmitAuditEvent(events.UserLoginEvent, events.EventFields{
				events.LoginMethod:        events.LoginMethodOIDC,
				events.AuthAttemptMessage: warningMessage,
			})
			break collect
		}
		response, err := g.fetchGroupsPage(nextPageToken)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		groups = append(groups, response.groups()...)
		if response.NextPageToken == "" {
			break collect
		}
		count++
		nextPageToken = response.NextPageToken
	}
	return jose.Claims{"groups": groups}, nil
}

func (g *gsuiteClient) fetchGroupsPage(pageToken string) (*gsuiteGroups, error) {
	// copy URL to avoid modifying the same url
	// with query parameters
	u := g.url
	q := u.Query()
	q.Set("userKey", g.userEmail)
	if pageToken != "" {
		q.Set("pageToken", pageToken)
	}
	u.RawQuery = q.Encode()
	endpoint := u.String()

	log.Debugf("Fetching OIDC claims from GSuite groups endpoint: %q.", endpoint)

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", g.accessToken))

	resp, err := g.Do(req)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer resp.Body.Close()

	bytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, trace.AccessDenied("bad status code: %v %v", resp.StatusCode, string(bytes))
	}
	var response gsuiteGroups
	if err := json.Unmarshal(bytes, &response); err != nil {
		return nil, trace.BadParameter("failed to parse response: %v", err)
	}
	return &response, nil
}

type gsuiteGroups struct {
	NextPageToken string        `json:"nextPageToken"`
	Groups        []gsuiteGroup `json:"groups"`
}

func (g gsuiteGroups) groups() []string {
	groups := make([]string, len(g.Groups))
	for i, group := range g.Groups {
		groups[i] = group.Email
	}
	return groups
}

type gsuiteGroup struct {
	Email string `json:"email"`
}

// mergeClaims merges b into a.
func mergeClaims(a jose.Claims, b jose.Claims) (jose.Claims, error) {
	for k, v := range b {
		_, ok := a[k]
		if !ok {
			a[k] = v
		}
	}

	return a, nil
}

// getClaims gets claims from ID token and UserInfo and returns UserInfo claims merged into ID token claims.
func (a *AuthServer) getClaims(oidcClient *oidc.Client, issuerURL string, scope []string, code string) (jose.Claims, error) {
	var err error

	oac, err := oidcClient.OAuthClient()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	t, err := oac.RequestToken(oauth2.GrantTypeAuthCode, code)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	idTokenClaims, err := claimsFromIDToken(oidcClient, t.IDToken)
	if err != nil {
		log.Debugf("Unable to fetch OIDC ID token claims: %v.", err)
		return nil, trace.Wrap(err)
	}
	log.Debugf("OIDC ID Token claims: %v.", idTokenClaims)

	userInfoClaims, err := claimsFromUserInfo(oidcClient, issuerURL, t.AccessToken)
	if err != nil {
		if trace.IsNotFound(err) {
			log.Debugf("OIDC provider doesn't offer UserInfo endpoint. Returning token claims: %v.", idTokenClaims)
			return idTokenClaims, nil
		}
		log.Debugf("Unable to fetch UserInfo claims: %v.", err)
		return nil, trace.Wrap(err)
	}
	log.Debugf("UserInfo claims: %v.", userInfoClaims)

	// make sure that the subject in the userinfo claim matches the subject in
	// the id token otherwise there is the possibility of a token substitution attack.
	// see section 16.11 of the oidc spec for more details.
	var idsub string
	var uisub string
	var exists bool
	if idsub, exists, err = idTokenClaims.StringClaim("sub"); err != nil || !exists {
		log.Debugf("Unable to extract OIDC sub claim from ID token.")
		return nil, trace.Wrap(err)
	}
	if uisub, exists, err = userInfoClaims.StringClaim("sub"); err != nil || !exists {
		log.Debugf("Unable to extract OIDC sub claim from UserInfo.")
		return nil, trace.Wrap(err)
	}
	if idsub != uisub {
		log.Debugf("OIDC claim subjects don't match '%v' != '%v'.", idsub, uisub)
		return nil, trace.BadParameter("invalid subject in UserInfo")
	}

	claims, err := mergeClaims(idTokenClaims, userInfoClaims)
	if err != nil {
		log.Debugf("Unable to merge OIDC claims: %v.", err)
		return nil, trace.Wrap(err)
	}

	// for GSuite users, fetch extra data from the proprietary google API
	// only if scope includes admin groups readonly scope
	if issuerURL == teleport.GSuiteIssuerURL && utils.SliceContainsStr(scope, teleport.GSuiteGroupsScope) {
		email, _, err := claims.StringClaim("email")
		if err != nil {
			return nil, trace.Wrap(err)
		}
		gsuiteClaims, err := a.claimsFromGSuite(oidcClient, issuerURL, email, t.AccessToken)
		if err != nil {
			if !trace.IsNotFound(err) {
				return nil, trace.Wrap(err)
			}
			log.Debugf("Found no GSuite claims.")
		} else {
			if gsuiteClaims != nil {
				log.Debugf("Got GSuiteclaims: %v.", gsuiteClaims)
			}
			claims, err = mergeClaims(claims, gsuiteClaims)
			if err != nil {
				return nil, trace.Wrap(err)
			}
		}
	}

	return claims, nil
}

// validateACRValues validates that we get an appropriate response for acr values. By default
// we expect the same value we send, but this function also handles Identity Provider specific
// forms of validation.
func (a *AuthServer) validateACRValues(acrValue string, identityProvider string, claims jose.Claims) error {
	switch identityProvider {
	case teleport.NetIQ:
		log.Debugf("Validating OIDC ACR values with '%v' rules.", identityProvider)

		tokenAcr, ok := claims["acr"]
		if !ok {
			return trace.BadParameter("acr not found in claims")
		}
		tokenAcrMap, ok := tokenAcr.(map[string]interface{})
		if !ok {
			return trace.BadParameter("acr unexpected type: %T", tokenAcr)
		}
		tokenAcrValues, ok := tokenAcrMap["values"]
		if !ok {
			return trace.BadParameter("acr.values not found in claims")
		}
		tokenAcrValuesSlice, ok := tokenAcrValues.([]interface{})
		if !ok {
			return trace.BadParameter("acr.values unexpected type: %T", tokenAcr)
		}

		acrValueMatched := false
		for _, v := range tokenAcrValuesSlice {
			vv, ok := v.(string)
			if !ok {
				continue
			}
			if acrValue == vv {
				acrValueMatched = true
				break
			}
		}
		if !acrValueMatched {
			log.Debugf("No OIDC ACR match found for '%v' in '%v'.", acrValue, tokenAcrValues)
			return trace.BadParameter("acr claim does not match")
		}
	default:
		log.Debugf("Validating OIDC ACR values with default rules.")

		claimValue, exists, err := claims.StringClaim("acr")
		if !exists {
			return trace.BadParameter("acr claim does not exist")
		}
		if err != nil {
			return trace.Wrap(err)
		}
		if claimValue != acrValue {
			log.Debugf("No OIDC ACR match found '%v' != '%v'.", acrValue, claimValue)
			return trace.BadParameter("acr claim does not match")
		}
	}

	return nil
}
