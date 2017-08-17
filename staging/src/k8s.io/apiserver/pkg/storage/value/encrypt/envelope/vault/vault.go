/*
Copyright 2017 The Kubernetes Authors.

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

// Package vault implements envelop encryption provider based on Vault KMS


package vault

import (
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"strings"

	"github.com/ghodss/yaml"

	"k8s.io/apiserver/pkg/storage/value/encrypt/envelope"
)

// VaultConfig contains connection information for Vault transformer
type VaultEnvelopeConfig struct {
	// The names of encryption key for Vault transit communication
	KeyNames []string `json:"key-names"`
	// Vault listen address, for example https://localhost:8200
	Address string `json:"addr"`

	// Token authentication information
	Token string `json:"token"`

	// TLS certificate authentication information
	ClientCert string `json:"client-cert"`
	ClientKey  string `json:"client-key"`

	// AppRole authentication information
	RoleId   string `json:"role-id"`
	SecretId string `json:"secret-id"`

	// CACert is the path to a PEM-encoded CA cert file to use to verify the
	// Vault server SSL certificate.
	CACert string `json:"ca-cert"`

	// TLSServerName, if set, is used to set the SNI host when connecting via TLS.
	TLSServerName string `json:"tls-server-name"`

	// The path for transit API, default is "transit"
	TransitPath string `json:"transit-path"`

	// The path for auth backend, default is "auth"
	AuthPath string `json:"auth-path"`
}

// Factory function that create Vault KMS service
func VaultKMSFactory(configFile io.Reader) (envelope.Service, error) {
	configFileContents, err := ioutil.ReadAll(configFile)
	if err != nil {
		return nil, fmt.Errorf("could not read contents: %v", err)
	}

	var config VaultEnvelopeConfig
	err = yaml.Unmarshal(configFileContents, &config)
	if err != nil {
		return nil, fmt.Errorf("error while parsing file: %v", err)
	}

	err = checkConfig(&config)
	if err != nil {
		return nil, err
	}

	client, err := newClientWrapper(&config)
	if err != nil {
		return nil, err
	}

	return &vaultEnvelopeService{config: &config, client: client}, nil
}

func checkConfig(config *VaultEnvelopeConfig) error {
	if len(config.KeyNames) == 0 {
		return fmt.Errorf("vault provider has no valid key names")
	}

	if config.Address == "" {
		return fmt.Errorf("vault provider has no valid address")
	}

	return checkAuthConfig(config)
}

func checkAuthConfig(config *VaultEnvelopeConfig) error {
	var count uint

	if config.Token != "" {
		count++
	}

	if config.ClientCert != "" || config.ClientKey != "" {
		if config.ClientCert == "" || config.ClientKey == "" {
			return fmt.Errorf("vault provider has invalid TLS authentication information")
		}
		count++
	}

	if config.RoleId != "" || config.SecretId != "" {
		if config.RoleId == "" {
			return fmt.Errorf("vault provider has invalid approle authentication information")
		}
		count++
	}

	if count == 0 {
		return fmt.Errorf("vault provider has no authentication information")
	}
	if count > 1 {
		return fmt.Errorf("vault provider has more than one authentication information")
	}

	return nil
}

type vaultEnvelopeService struct {
	config *VaultEnvelopeConfig
	client *clientWrapper
}

func (s *vaultEnvelopeService) Decrypt(data string) ([]byte, error) {
	// Find the mached key
	var key string
	for _, name := range s.config.KeyNames {
		if strings.HasPrefix(data, name+":") {
			key = name
			break
		}
	}
	if key == "" {
		return nil, fmt.Errorf("no matching vault key found")
	}

	// Replace the key name with "vault:" for Vault transit API
	cipher := strings.Replace(data, key, "vault", 1)

	//plain, _, err := s.client.decrypt(key, cipher)
	plain, err := s.withRefreshToken((*clientWrapper).decrypt, key, cipher)
	if err != nil {
		return nil, err
	}

	return base64.StdEncoding.DecodeString(plain)
}

func (s *vaultEnvelopeService) Encrypt(data []byte) (string, error) {
	// Use the frist key to encrypt
	key := s.config.KeyNames[0]
	plain := base64.StdEncoding.EncodeToString(data)

	cipher, err := s.withRefreshToken((*clientWrapper).encrypt, key, plain)
	if err != nil {
		return "", err
	}

	// The format of cipher from Vault is "vault:v1:....".
	// "vault:" is unnecessary, replace it with key name.
	return strings.Replace(cipher, "vault", key, 1), nil
}

// The function type for clientWrapper.encrypt and clientWrapper.decrypt.
type encryptOrDecryptFunc func(*clientWrapper, string, string) (string, error)

func (s *vaultEnvelopeService) withRefreshToken(f encryptOrDecryptFunc, key, data string) (string, error) {
	// Execute operation first time.
	result, err := f(s.client, key, data)
	if err == nil || s.config.Token != "" {
		return result, err
	}

	forbidden, ok := err.(*forbiddenError)
	if !ok {
		return result, err
	}

	// The request is forbidden, refresh token and execute operation again.
	err = s.client.refreshToken(s.config, forbidden.version)
	if err != nil {
		return result, err
	}

	return f(s.client, key, data)
}
