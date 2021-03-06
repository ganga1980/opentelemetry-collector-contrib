// Copyright 2020, OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubelet

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"

	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/common/k8sconfig"
)

const svcAcctCACertPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
const svcAcctTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"

type Client interface {
	Get(path string) ([]byte, error)
}

func NewClient(endpoint string, cfg *ClientConfig, logger *zap.Logger) (Client, error) {
	switch cfg.APIConfig.AuthType {
	case k8sconfig.AuthTypeTLS:
		return newTLSClient(endpoint, cfg, logger)
	case k8sconfig.AuthTypeServiceAccount:
		return newServiceAccountClient(endpoint, svcAcctCACertPath, svcAcctTokenPath, logger)
	default:
		return nil, fmt.Errorf("AuthType [%s] not supported", cfg.APIConfig.AuthType)
	}
}

func newTLSClient(endpoint string, cfg *ClientConfig, logger *zap.Logger) (Client, error) {
	rootCAs, err := systemCertPoolPlusPath(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	clientCert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	return defaultTLSClient(
		endpoint,
		cfg.InsecureSkipVerify,
		rootCAs,
		[]tls.Certificate{clientCert},
		nil,
		logger,
	)
}

func newServiceAccountClient(
	endpoint string,
	caCertPath string,
	tokenPath string,
	logger *zap.Logger,
) (*clientImpl, error) {
	rootCAs, err := systemCertPoolPlusPath(caCertPath)
	if err != nil {
		return nil, err
	}
	tok, err := ioutil.ReadFile(tokenPath)
	if err != nil {
		return nil, err
	}
	tr := defaultTransport()
	tr.TLSClientConfig = &tls.Config{
		RootCAs: rootCAs,
	}
	return defaultTLSClient(endpoint, true, rootCAs, nil, tok, logger)
}

func defaultTLSClient(
	endpoint string,
	insecureSkipVerify bool,
	rootCAs *x509.CertPool,
	certificates []tls.Certificate,
	tok []byte,
	logger *zap.Logger,
) (*clientImpl, error) {
	tr := defaultTransport()
	tr.TLSClientConfig = &tls.Config{
		RootCAs:            rootCAs,
		Certificates:       certificates,
		InsecureSkipVerify: insecureSkipVerify,
	}
	if endpoint == "" {
		var err error
		endpoint, err = defaultEndpoint()
		if err != nil {
			return nil, err
		}
		logger.Warn("Kubelet endpoint not defined, using default endpoint " + endpoint)
	}
	return &clientImpl{
		baseURL:    "https://" + endpoint,
		httpClient: http.Client{Transport: tr},
		tok:        tok,
		logger:     logger,
	}, nil
}

// This will work if hostNetwork is turned on, in which case the pod has access
// to the node's loopback device.
// https://kubernetes.io/docs/concepts/policy/pod-security-policy/#host-namespaces
func defaultEndpoint() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", errors.WithMessage(err, "Unable to get hostname for default endpoint")
	}
	const kubeletPort = "10250"
	return hostname + ":" + kubeletPort, nil
}

func defaultTransport() *http.Transport {
	return http.DefaultTransport.(*http.Transport).Clone()
}

// clientImpl

var _ Client = (*clientImpl)(nil)

type clientImpl struct {
	baseURL    string
	httpClient http.Client
	logger     *zap.Logger
	tok        []byte
}

func (c *clientImpl) Get(path string) ([]byte, error) {
	req, err := c.buildReq(path)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		closeErr := resp.Body.Close()
		if closeErr != nil {
			c.logger.Warn("failed to close response body", zap.Error(closeErr))
		}
	}()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (c *clientImpl) buildReq(path string) (*http.Request, error) {
	url := c.baseURL + path
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.tok != nil {
		req.Header.Set("Authorization", fmt.Sprintf("bearer %s", c.tok))
	}
	return req, nil
}
