// Copyright Jetstack Ltd. See LICENSE for details.
package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/request/bearertoken"
	authuser "k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/plugin/pkg/authenticator/token/oidc"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"k8s.io/klog"

	"github.com/jetstack/kube-oidc-proxy/cmd/app/options"
	"github.com/jetstack/kube-oidc-proxy/pkg/proxy/tokenreview"
)

const (
	UserHeaderClientIPKey = "Remote-Client-IP"
)

var (
	errUnauthorized      = errors.New("Unauthorized")
	errImpersonateHeader = errors.New("Impersonate-User in header")
	errNoName            = errors.New("No name in OIDC info")

	// http headers are case-insensitive
	impersonateUserHeader  = strings.ToLower(transport.ImpersonateUserHeader)
	impersonateGroupHeader = strings.ToLower(transport.ImpersonateGroupHeader)
	impersonateExtraHeader = strings.ToLower(transport.ImpersonateUserExtraHeaderPrefix)
)

type Options struct {
	DisableImpersonation bool
	TokenReview          bool

	ExtraUserHeaders                map[string][]string
	ExtraUserHeadersClientIPEnabled bool
}

type Proxy struct {
	oidcRequestAuther *bearertoken.Authenticator
	tokenAuther       authenticator.Token
	tokenReviewer     *tokenreview.TokenReview
	secureServingInfo *server.SecureServingInfo

	restConfig            *rest.Config
	clientTransport       http.RoundTripper
	noAuthClientTransport http.RoundTripper

	options *Options
}

func New(restConfig *rest.Config, oidcOptions *options.OIDCAuthenticationOptions,
	tokenReviewer *tokenreview.TokenReview, ssinfo *server.SecureServingInfo,
	options *Options) (*Proxy, error) {

	// generate tokenAuther from oidc config
	tokenAuther, err := oidc.New(oidc.Options{
		APIAudiences:         oidcOptions.APIAudiences,
		CAFile:               oidcOptions.CAFile,
		ClientID:             oidcOptions.ClientID,
		GroupsClaim:          oidcOptions.GroupsClaim,
		GroupsPrefix:         oidcOptions.GroupsPrefix,
		IssuerURL:            oidcOptions.IssuerURL,
		RequiredClaims:       oidcOptions.RequiredClaims,
		SupportedSigningAlgs: oidcOptions.SigningAlgs,
		UsernameClaim:        oidcOptions.UsernameClaim,
		UsernamePrefix:       oidcOptions.UsernamePrefix,
	})
	if err != nil {
		return nil, err
	}

	return &Proxy{
		restConfig:        restConfig,
		tokenReviewer:     tokenReviewer,
		secureServingInfo: ssinfo,
		options:           options,
		oidcRequestAuther: bearertoken.New(tokenAuther),
		tokenAuther:       tokenAuther,
	}, nil
}

func (p *Proxy) Run(stopCh <-chan struct{}) (<-chan struct{}, error) {
	// standard round tripper for proxy to API Server
	clientRT, err := p.roundTripperForRestConfig(p.restConfig)
	if err != nil {
		return nil, err
	}
	p.clientTransport = clientRT

	// No auth round tripper for no impersonation
	if p.options.DisableImpersonation || p.options.TokenReview {
		noAuthClientRT, err := p.roundTripperForRestConfig(&rest.Config{
			APIPath: p.restConfig.APIPath,
			Host:    p.restConfig.Host,
			Timeout: p.restConfig.Timeout,
			TLSClientConfig: rest.TLSClientConfig{
				CAFile: p.restConfig.CAFile,
				CAData: p.restConfig.CAData,
			},
		})
		if err != nil {
			return nil, err
		}

		p.noAuthClientTransport = noAuthClientRT
	}

	// get API server url
	url, err := url.Parse(p.restConfig.Host)
	if err != nil {
		return nil, fmt.Errorf("failed to parse url: %s", err)
	}

	// set up proxy handler using proxy
	proxyHandler := httputil.NewSingleHostReverseProxy(url)
	proxyHandler.Transport = p
	proxyHandler.ErrorHandler = p.Error

	waitCh, err := p.serve(proxyHandler, stopCh)
	if err != nil {
		return nil, err
	}

	return waitCh, nil
}

func (p *Proxy) serve(proxyHandler *httputil.ReverseProxy, stopCh <-chan struct{}) (<-chan struct{}, error) {
	// securely serve using serving config
	waitCh, err := p.secureServingInfo.Serve(proxyHandler, time.Second*60, stopCh)
	if err != nil {
		return nil, err
	}

	return waitCh, nil
}

func (p *Proxy) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request here since successfully authenticating the request
	// deletes those auth headers
	reqCpy := utilnet.CloneRequest(req)

	// auth request and handle unauthed
	info, ok, err := p.oidcRequestAuther.AuthenticateRequest(reqCpy)
	klog.V(2).Infof("AuthenticateRequest failed with err: %s", err)
	if err != nil {

		// attempt to passthrough request if valid token
		if p.options.TokenReview {
			return p.tokenReview(reqCpy)
		}

		return nil, errUnauthorized
	}

	// failed authorization
	if !ok {
		return nil, errUnauthorized
	}

	klog.V(4).Infof("authenticated request: %s", reqCpy.RemoteAddr)

	// if we have disabled impersonation we can forward the request right away
	if p.options.DisableImpersonation {
		klog.V(2).Infof("passing on request with no impersonation: %s", reqCpy.RemoteAddr)
		// Send original copy here with auth header intact
		return p.noAuthClientTransport.RoundTrip(req)
	}

	// check for incoming impersonation headers and reject if any exists
	if p.hasImpersonation(reqCpy.Header) {
		return nil, errImpersonateHeader
	}

	user := info.User

	// no name available so reject request
	if user.GetName() == "" {
		return nil, errNoName
	}

	// ensure group contains allauthenticated builtin
	found := false
	groups := user.GetGroups()
	for _, elem := range groups {
		if elem == authuser.AllAuthenticated {
			found = true
			break
		}
	}
	if !found {
		groups = append(groups, authuser.AllAuthenticated)
	}

	extra := user.GetExtra()

	if extra == nil {
		extra = make(map[string][]string)
	}

	// If client IP user extra header option set then append the remote client
	// address.
	if p.options.ExtraUserHeadersClientIPEnabled {
		klog.V(6).Infof("adding impersonate extra user header %s: %s (%s)",
			UserHeaderClientIPKey, req.RemoteAddr, reqCpy.RemoteAddr)

		extra[UserHeaderClientIPKey] = append(extra[UserHeaderClientIPKey], req.RemoteAddr)
	}

	// Add custom extra user headers to impersonation request
	for k, vs := range p.options.ExtraUserHeaders {
		for _, v := range vs {
			klog.V(6).Infof("adding impersonate extra user header %s: %s (%s)",
				k, v, reqCpy.RemoteAddr)

			extra[k] = append(extra[k], v)
		}
	}

	// Set impersonation header using authenticated user identity.
	conf := transport.ImpersonationConfig{
		UserName: user.GetName(),
		Groups:   groups,
		Extra:    extra,
	}

	rt := transport.NewImpersonatingRoundTripper(conf, p.clientTransport)

	// push request through round trippers to the API server
	return rt.RoundTrip(reqCpy)
}

func (p *Proxy) tokenReview(req *http.Request) (*http.Response, error) {
	klog.V(4).Infof("attempting to validate a token in request using TokenReview endpoint(%s)",
		req.RemoteAddr)

	ok, err := p.tokenReviewer.Review(req)
	// no error so passthrough the request
	if err == nil && ok {
		klog.V(4).Infof("passing request with valid token through (%s)",
			req.RemoteAddr)
		// Don't set impersonation headers and pass through without proxy auth
		// and headers still set
		return p.noAuthClientTransport.RoundTrip(req)
	}

	if err != nil {
		klog.Errorf("unable to authenticate the request via TokenReview due to an error (%s): %s",
			req.RemoteAddr, err)
	}

	return nil, errUnauthorized
}

func (p *Proxy) hasImpersonation(header http.Header) bool {
	for h := range header {
		if strings.ToLower(h) == impersonateUserHeader ||
			strings.ToLower(h) == impersonateGroupHeader ||
			strings.HasPrefix(strings.ToLower(h), impersonateExtraHeader) {

			return true
		}
	}

	return false
}

func (p *Proxy) Error(rw http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		klog.Error("error was called with no error")
		http.Error(rw, "", http.StatusInternalServerError)
		return
	}

	switch err {

	// failed auth
	case errUnauthorized:
		klog.V(2).Infof("unauthenticated user request %s", r.RemoteAddr)
		http.Error(rw, "Unauthorized", http.StatusUnauthorized)
		return

		// user request with impersonation
	case errImpersonateHeader:
		klog.V(2).Infof("impersonation user request %s", r.RemoteAddr)

		http.Error(rw, "Impersonation requests are disabled when using kube-oidc-proxy", http.StatusForbidden)
		return

		// no name given or available in oidc response
	case errNoName:
		klog.V(2).Infof("no name available in oidc info %s", r.RemoteAddr)
		http.Error(rw, "Username claim not available in OIDC Issuer response", http.StatusForbidden)
		return

		// server or unknown error
	default:
		klog.Errorf("unknown error (%s): %s", r.RemoteAddr, err)
		http.Error(rw, "", http.StatusInternalServerError)
	}
}

func (p *Proxy) roundTripperForRestConfig(config *rest.Config) (http.RoundTripper, error) {
	// get golang tls config to the API server
	tlsConfig, err := rest.TLSConfigFor(config)
	if err != nil {
		return nil, err
	}

	// create tls transport to request
	tlsTransport := &http.Transport{
		TLSClientConfig: tlsConfig,
	}

	// get kube transport config form rest client config
	restTransportConfig, err := config.TransportConfig()
	if err != nil {
		return nil, err
	}

	// wrap golang tls config with kube transport round tripper
	clientRT, err := transport.HTTPWrappersForConfig(restTransportConfig, tlsTransport)
	if err != nil {
		return nil, err
	}

	return clientRT, nil
}

// Return the proxy OIDC token authenticator
func (p *Proxy) OIDCTokenAuthenticator() authenticator.Token {
	return p.tokenAuther
}
