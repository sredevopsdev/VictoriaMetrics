package kubernetes

import (
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/netutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promauth"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/promscrape/discoveryutils"
	"github.com/VictoriaMetrics/fasthttp"
)

// apiConfig contains config for API server
type apiConfig struct {
	client     *fasthttp.HostClient
	server     string
	hostPort   string
	authConfig *promauth.Config
	namespaces []string
	selectors  []Selector
}

var configMap = discoveryutils.NewConfigMap()

func getAPIConfig(sdc *SDConfig, baseDir string) (*apiConfig, error) {
	v, err := configMap.Get(sdc, func() (interface{}, error) { return newAPIConfig(sdc, baseDir) })
	if err != nil {
		return nil, err
	}
	return v.(*apiConfig), nil
}

func newAPIConfig(sdc *SDConfig, baseDir string) (*apiConfig, error) {
	ac, err := promauth.NewConfig(baseDir, sdc.BasicAuth, sdc.BearerToken, sdc.BearerTokenFile, sdc.TLSConfig)
	if err != nil {
		return nil, fmt.Errorf("cannot parse auth config: %s", err)
	}
	hcv, err := newHostClient(sdc.APIServer, ac)
	if err != nil {
		return nil, fmt.Errorf("cannot create HTTP client for %q: %s", sdc.APIServer, err)
	}
	cfg := &apiConfig{
		client:     hcv.hc,
		server:     hcv.apiServer,
		hostPort:   hcv.hostPort,
		authConfig: hcv.ac,
		namespaces: sdc.Namespaces.Names,
		selectors:  sdc.Selectors,
	}
	return cfg, nil
}

func getAPIResponse(cfg *apiConfig, role, path string) ([]byte, error) {
	query := joinSelectors(role, cfg.namespaces, cfg.selectors)
	if len(query) > 0 {
		path += "?" + query
	}
	requestURL := cfg.server + path
	var u fasthttp.URI
	u.Update(requestURL)
	var req fasthttp.Request
	req.SetRequestURIBytes(u.RequestURI())
	req.SetHost(cfg.hostPort)
	req.Header.Set("Accept-Encoding", "gzip")
	if cfg.authConfig != nil && cfg.authConfig.Authorization != "" {
		req.Header.Set("Authorization", cfg.authConfig.Authorization)
	}
	var resp fasthttp.Response
	// There is no need in calling DoTimeout, since the timeout is already set in hc.ReadTimeout above.
	if err := cfg.client.Do(&req, &resp); err != nil {
		return nil, fmt.Errorf("cannot fetch %q: %s", requestURL, err)
	}
	var data []byte
	if ce := resp.Header.Peek("Content-Encoding"); string(ce) == "gzip" {
		dst, err := fasthttp.AppendGunzipBytes(nil, resp.Body())
		if err != nil {
			return nil, fmt.Errorf("cannot ungzip response from %q: %s", requestURL, err)
		}
		data = dst
	} else {
		data = append(data[:0], resp.Body()...)
	}
	statusCode := resp.StatusCode()
	if statusCode != fasthttp.StatusOK {
		return nil, fmt.Errorf("unexpected status code returned from %q: %d; expecting %d; response body: %q",
			requestURL, statusCode, fasthttp.StatusOK, data)
	}
	return data, nil
}

type hcValue struct {
	hc        *fasthttp.HostClient
	ac        *promauth.Config
	apiServer string
	hostPort  string
}

func newHostClient(apiServer string, ac *promauth.Config) (*hcValue, error) {
	if len(apiServer) == 0 {
		// Assume we run at k8s pod.
		// Discover apiServer and auth config according to k8s docs.
		// See https://kubernetes.io/docs/reference/access-authn-authz/service-accounts-admin/#service-account-admission-controller
		host := os.Getenv("KUBERNETES_SERVICE_HOST")
		port := os.Getenv("KUBERNETES_SERVICE_PORT")
		if len(host) == 0 {
			return nil, fmt.Errorf("cannot find KUBERNETES_SERVICE_HOST env var; it must be defined when running in k8s; " +
				"probably, `kubernetes_sd_config->api_server` is missing in Prometheus configs?")
		}
		if len(port) == 0 {
			return nil, fmt.Errorf("cannot find KUBERNETES_SERVICE_PORT env var; it must be defined when running in k8s; "+
				"KUBERNETES_SERVICE_HOST=%q", host)
		}
		apiServer = "https://" + net.JoinHostPort(host, port)
		tlsConfig := promauth.TLSConfig{
			CAFile: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
		}
		acNew, err := promauth.NewConfig("/", nil, "", "/var/run/secrets/kubernetes.io/serviceaccount/token", &tlsConfig)
		if err != nil {
			return nil, fmt.Errorf("cannot initialize service account auth: %s; probably, `kubernetes_sd_config->api_server` is missing in Prometheus configs?", err)
		}
		ac = acNew
	}

	var u fasthttp.URI
	u.Update(apiServer)
	hostPort := string(u.Host())
	isTLS := string(u.Scheme()) == "https"
	var tlsCfg *tls.Config
	if isTLS && ac != nil {
		tlsCfg = ac.NewTLSConfig()
	}
	if !strings.Contains(hostPort, ":") {
		port := "80"
		if isTLS {
			port = "443"
		}
		hostPort = net.JoinHostPort(hostPort, port)
	}
	hc := &fasthttp.HostClient{
		Addr:                hostPort,
		Name:                "vm_promscrape/discovery",
		DialDualStack:       netutil.TCP6Enabled(),
		IsTLS:               isTLS,
		TLSConfig:           tlsCfg,
		ReadTimeout:         time.Minute,
		WriteTimeout:        10 * time.Second,
		MaxResponseBodySize: 300 * 1024 * 1024,
	}
	return &hcValue{
		hc:        hc,
		ac:        ac,
		apiServer: apiServer,
		hostPort:  hostPort,
	}, nil
}
