package core

import (
	"crypto/rsa"
	"crypto/tls"
	"fmt"
	jwt "github.com/dgrijalva/jwt-go"
	"github.com/gorilla/sessions"
	"github.com/mendsley/gojwk"
	"github.com/patrickmn/go-cache"
	"golang.org/x/net/context"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	"io/ioutil"
	"net/http"
	"time"
)

var encryptionkey = "something-very-secret"

type IDPConfig struct {
	ClientID       string `yaml:"client_id"`
	ClientSecret   string `yaml:"client_secret"`
	HydraAddress   string `yaml:"token_endpoint"`
	ChallengeStore sessions.Store
}

type IDP struct {
	config *IDPConfig

	// Http client for communicating with Hydra
	client *http.Client

	// Key for challenge JWT verification
	verificationKey *rsa.PublicKey

	// Key for signing the consent JWT
	consentKey *rsa.PrivateKey
	keyCache   *cache.Cache
}

func NewIDP(config *IDPConfig) *IDP {
	var idp = new(IDP)
	idp.config = config

	// TODO: Pass TTL and refresh period from config
	idp.keyCache = cache.New(5*time.Minute, 30*time.Second)
	return idp
}

// Gets the requested key from Hydra
func (idp *IDP) getKey(set string, kind string) (*gojwk.Key, error) {
	url := idp.config.HydraAddress + "/keys/" + set + "/" + kind

	resp, err := idp.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	key, err := gojwk.Unmarshal(body)
	if err != nil {
		return nil, err
	}

	return key.Keys[0], nil
}

// Downloads the hydra's public key
func (idp *IDP) getVerificationKey() error {
	jwk, err := idp.getKey("consent.challenge", "public")
	if err != nil {
		return err
	}

	key, err := jwk.DecodePublicKey()
	if err != nil {
		return err
	}

	idp.verificationKey = key.(*rsa.PublicKey)

	return err
}

// Downloads the private key used for signing the consent
func (idp *IDP) getConsentKey() error {
	jwk, err := idp.getKey("consent.endpoint", "private")
	if err != nil {
		return err
	}

	key, err := jwk.DecodePrivateKey()
	if err != nil {
		return err
	}

	idp.consentKey = key.(*rsa.PrivateKey)

	return err
}

func (idp *IDP) login() error {
	// Use the credentials to login to Hydra
	credentials := clientcredentials.Config{
		ClientID:     idp.config.ClientID,
		ClientSecret: idp.config.ClientSecret,
		TokenURL:     idp.config.HydraAddress + "/oauth2/token",
		Scopes:       []string{"core", "hydra.keys.get"},
	}

	// Skip verifying the certificate
	// TODO: Remove when Hydra implements passing key-cert pairs
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	c := &http.Client{Transport: tr}
	ctx := context.WithValue(oauth2.NoContext, oauth2.HTTPClient, c)

	// Prefetch the token - tests the connection``
	_, err := credentials.Token(ctx)
	if err != nil {
		return err
	}

	idp.client = credentials.Client(ctx)

	return nil
}

func (idp *IDP) Connect() error {
	err := idp.login()
	if err != nil {
		return err
	}

	err = idp.getVerificationKey()
	if err != nil {
		return err
	}

	err = idp.getConsentKey()
	if err != nil {
		return err
	}

	return err
}

// Parse and verify the challenge JWT
func (idp *IDP) getChallengeToken(challengeString string) (*jwt.Token, error) {
	token, err := jwt.Parse(challengeString, func(token *jwt.Token) (interface{}, error) {
		_, ok := token.Method.(*jwt.SigningMethodRSA)
		if !ok {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
		}

		return idp.verificationKey, nil
	})

	if err != nil {
		return nil, err
	}

	if !token.Valid {
		return nil, fmt.Errorf("Empty token")
	}

	return token, nil
}

func (idp *IDP) GetConsentKey() (*rsa.PrivateKey, error) {
	return idp.consentKey, nil
}

func (idp *IDP) NewChallenge(r *http.Request) (challenge *Challenge, err error) {
	tokenStr := r.FormValue("challenge")
	if tokenStr == "" {
		// No challenge token
		err = ErrorBadRequest
		return
	}

	token, err := idp.getChallengeToken(tokenStr)
	if err != nil {
		return
	}

	challenge = new(Challenge)
	challenge.idp = idp

	// Get data from the challenge jwt
	claims := token.Claims.(jwt.MapClaims)
	challenge.Client = claims["aud"].(string)
	challenge.Redirect = claims["redir"].(string)
	challenge.Expires = time.Unix(int64(claims["exp"].(float64)), 0)

	scopes := claims["scp"].([]interface{})
	challenge.Scopes = make([]string, len(scopes), len(scopes))
	for i, scope := range scopes {
		challenge.Scopes[i] = scope.(string)
	}

	return
}

func (idp *IDP) GetChallenge(r *http.Request) (*Challenge, error) {
	session, err := idp.config.ChallengeStore.Get(r, SessionCookieName)
	if err != nil {
		return nil, err
	}

	challenge, ok := session.Values[SessionCookieName].(*Challenge)
	if !ok {
		return nil, ErrorBadChallengeCookie
	}

	challenge.idp = idp

	return challenge, nil
}

// Generate the consent
func (idp *IDP) generateConsentToken(challenge *jwt.Token, subject string, scopes []string) (string, error) {
	now := time.Now()

	token := jwt.New(jwt.SigningMethodRS256)
	claims := token.Claims.(jwt.MapClaims)
	claims["aud"] = challenge.Claims.(jwt.MapClaims)["aud"]
	claims["exp"] = now.Add(time.Minute * 5).Unix()
	claims["iat"] = now.Unix()
	claims["scp"] = scopes
	claims["sub"] = subject

	// Sign and get the complete encoded token as a string
	tokenString, err := token.SignedString(idp.consentKey)
	if err != nil {
		return "", err
	}

	return tokenString, err
}

func (idp *IDP) Close() {
	fmt.Println("IDP closed")
	idp.client = nil
	idp.verificationKey = nil
	idp.consentKey = nil
}
