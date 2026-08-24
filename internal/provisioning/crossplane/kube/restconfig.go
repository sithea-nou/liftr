// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"time"
)

// RestConfig carries everything needed to reach one Kubernetes API server.
// Values live only inside the adapter: they are never logged, persisted, or
// exposed through any Liftr contract.
type RestConfig struct {
	Host            string
	BearerToken     string
	TokenFile       string
	Username        string
	Password        string
	CAData          []byte
	CertData        []byte
	KeyData         []byte
	InsecureSkipTLS bool
}

func (c *RestConfig) resolve() (*http.Client, error) {
	if c.Host == "" {
		return nil, fmt.Errorf("kubernetes api server host is required")
	}
	token := c.BearerToken
	if token == "" && c.TokenFile != "" {
		raw, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read bearer token file: %w", err)
		}
		token = string(raw)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: c.InsecureSkipTLS} //nolint:gosec // explicit operator opt-in
	if len(c.CAData) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(c.CAData) {
			return nil, fmt.Errorf("kubernetes certificate authority data is not valid PEM")
		}
		tlsConfig.RootCAs = pool
	}
	certData, keyData := c.CertData, c.KeyData
	if len(certData) > 0 && len(keyData) > 0 {
		certificate, err := tls.X509KeyPair(certData, keyData)
		if err != nil {
			return nil, fmt.Errorf("kubernetes client certificate is invalid: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{certificate}
	}
	transport.TLSClientConfig = tlsConfig
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	if token != "" || (c.Username != "" && c.Password != "") {
		client.Transport = &authRoundTripper{base: transport, token: token, username: c.Username, password: c.Password}
	}
	return client, nil
}

type authRoundTripper struct {
	base     http.RoundTripper
	token    string
	username string
	password string
}

func (a *authRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if a.token != "" {
		request.Header.Set("Authorization", "Bearer "+a.token)
	} else if a.username != "" || a.password != "" {
		request.SetBasicAuth(a.username, a.password)
	}
	return a.base.RoundTrip(request)
}

// InClusterEnvHost renders the in-cluster API server host from the standard
// service environment variables, or returns empty when they are absent.
func InClusterEnvHost() string {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return ""
	}
	return "https://" + host + ":" + port
}

const (
	inClusterTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	inClusterCAFile    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

// RestConfigInCluster builds the standard in-cluster configuration. It fails
// when the process is not running inside a Kubernetes pod.
func RestConfigInCluster() (*RestConfig, error) {
	host := InClusterEnvHost()
	if host == "" {
		return nil, fmt.Errorf("in-cluster configuration requires KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT")
	}
	token, err := os.ReadFile(inClusterTokenFile)
	if err != nil {
		return nil, fmt.Errorf("read in-cluster service account token: %w", err)
	}
	caData, err := os.ReadFile(inClusterCAFile)
	if err != nil {
		return nil, fmt.Errorf("read in-cluster certificate authority: %w", err)
	}
	return &RestConfig{Host: host, BearerToken: string(token), CAData: caData}, nil
}
