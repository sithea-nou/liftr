// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// kubeconfigDocument models the small, explicit subset of the kubeconfig
// format Liftr supports. Exec- and auth-provider-based credential plugins
// are deliberately unsupported and rejected at load time: they would add a
// subprocess and plugin boundary to the adapter that M14 does not need.
type kubeconfigDocument struct {
	CurrentContext string                 `yaml:"current-context"`
	Contexts       []kubeconfigContextRef `yaml:"contexts"`
	Clusters       []kubeconfigClusterRef `yaml:"clusters"`
	Users          []kubeconfigUserRef    `yaml:"users"`
}

type kubeconfigContextRef struct {
	Name    string            `yaml:"name"`
	Context kubeconfigContext `yaml:"context"`
}

type kubeconfigContext struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}

type kubeconfigClusterRef struct {
	Name    string            `yaml:"name"`
	Cluster kubeconfigCluster `yaml:"cluster"`
}

type kubeconfigCluster struct {
	Server                   string `yaml:"server"`
	CertificateAuthority     string `yaml:"certificate-authority"`
	CertificateAuthorityData string `yaml:"certificate-authority-data"`
	InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
}

type kubeconfigUserRef struct {
	Name string         `yaml:"name"`
	User kubeconfigUser `yaml:"user"`
}

type kubeconfigUser struct {
	Token                 string        `yaml:"token"`
	TokenFile             string        `yaml:"tokenFile"`
	Username              string        `yaml:"username"`
	Password              string        `yaml:"password"`
	ClientCertificate     string        `yaml:"client-certificate"`
	ClientCertificateData string        `yaml:"client-certificate-data"`
	ClientKey             string        `yaml:"client-key"`
	ClientKeyData         string        `yaml:"client-key-data"`
	Exec                  *yamlPresence `yaml:"exec"`
	AuthProvider          *yamlPresence `yaml:"auth-provider"`
}

type yamlPresence struct{}

// RestConfigFromKubeconfig loads one kubeconfig file and resolves the named
// context (or the file's current-context when empty) into a RestConfig.
func RestConfigFromKubeconfig(path, contextName string) (*RestConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read kubeconfig: %w", err)
	}
	var document kubeconfigDocument
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("parse kubeconfig: %w", err)
	}
	selected := contextName
	if selected == "" {
		selected = document.CurrentContext
	}
	if selected == "" {
		return nil, fmt.Errorf("kubeconfig has no current context")
	}
	var context *kubeconfigContext
	for index := range document.Contexts {
		if document.Contexts[index].Name == selected {
			context = &document.Contexts[index].Context
			break
		}
	}
	if context == nil {
		return nil, fmt.Errorf("kubeconfig context %q is not defined", selected)
	}
	config := &RestConfig{}
	for index := range document.Clusters {
		if document.Clusters[index].Name != context.Cluster {
			continue
		}
		cluster := document.Clusters[index].Cluster
		config.Host = cluster.Server
		config.InsecureSkipTLS = cluster.InsecureSkipTLSVerify
		if cluster.CertificateAuthorityData != "" {
			data, decodeErr := decodeBase64Field(cluster.CertificateAuthorityData)
			if decodeErr != nil {
				return nil, fmt.Errorf("kubeconfig cluster %q certificate-authority-data: %w", context.Cluster, decodeErr)
			}
			config.CAData = data
		} else if cluster.CertificateAuthority != "" {
			data, readErr := os.ReadFile(cluster.CertificateAuthority)
			if readErr != nil {
				return nil, fmt.Errorf("kubeconfig cluster %q certificate-authority: %w", context.Cluster, readErr)
			}
			config.CAData = data
		}
		break
	}
	for index := range document.Users {
		if document.Users[index].Name != context.User {
			continue
		}
		user := document.Users[index].User
		if user.Exec != nil {
			return nil, fmt.Errorf("kubeconfig user %q uses an exec credential plugin, which Liftr does not support", context.User)
		}
		if user.AuthProvider != nil {
			return nil, fmt.Errorf("kubeconfig user %q uses an auth-provider credential plugin, which Liftr does not support", context.User)
		}
		config.BearerToken = user.Token
		config.TokenFile = user.TokenFile
		config.Username = user.Username
		config.Password = user.Password
		if user.ClientCertificateData != "" && user.ClientKeyData != "" {
			certData, certErr := decodeBase64Field(user.ClientCertificateData)
			if certErr != nil {
				return nil, fmt.Errorf("kubeconfig user %q client-certificate-data: %w", context.User, certErr)
			}
			keyData, keyErr := decodeBase64Field(user.ClientKeyData)
			if keyErr != nil {
				return nil, fmt.Errorf("kubeconfig user %q client-key-data: %w", context.User, keyErr)
			}
			config.CertData, config.KeyData = certData, keyData
		} else if user.ClientCertificate != "" && user.ClientKey != "" {
			certData, certErr := os.ReadFile(user.ClientCertificate)
			if certErr != nil {
				return nil, fmt.Errorf("kubeconfig user %q client-certificate: %w", context.User, certErr)
			}
			keyData, keyErr := os.ReadFile(user.ClientKey)
			if keyErr != nil {
				return nil, fmt.Errorf("kubeconfig user %q client-key: %w", context.User, keyErr)
			}
			config.CertData, config.KeyData = certData, keyData
		}
		break
	}
	if config.Host == "" {
		return nil, fmt.Errorf("kubeconfig context %q does not resolve to a cluster server", selected)
	}
	return config, nil
}
