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
	"fmt"
	"strings"
	"sync"

	"github.com/hashicorp/vault/api"
)

// Handle all communication with Vault server.
type clientWrapper struct {
	client      *api.Client
	encryptPath string
	decryptPath string
	authPath    string

	// We may update token for api.Client, but there is no sync for api.Client.
	// Read lock for encrypt/decrypt requests, write lock for login requests which
	// will update token for api.Client.
	rwmutex sync.RWMutex
	version uint
}

// Initialize a client wrapper for vault kms provider.
func newClientWrapper(config *VaultEnvelopeConfig) (*clientWrapper, error) {
	client, err := newVaultClient(config)
	if err != nil {
		return nil, err
	}

	// Vault transit path is configurable. "path", "/path", "path/" and "/path/"
	// are the same.
	transit := "transit"
	if config.TransitPath != "" {
		transit = strings.Trim(config.TransitPath, "/")
	}

	// auth path is configurable. "path", "/path", "path/" and "/path/" are the same.
	auth := "auth/"
	if config.AuthPath != "" {
		auth = strings.Trim(config.AuthPath, "/")
	}
	wrapper := &clientWrapper{
		client:      client,
		encryptPath: "/v1/" + transit + "/encrypt/",
		decryptPath: "/v1/" + transit + "/decrypt/",
		authPath:    auth + "/",
	}

	// Set token for the api.client.
	if config.Token != "" {
		client.SetToken(config.Token)
	} else {
		err = wrapper.refreshToken(config, wrapper.version)
	}
	if err != nil {
		return nil, err
	}

	return wrapper, nil
}

func newVaultClient(config *VaultEnvelopeConfig) (*api.Client, error) {
	vaultConfig := api.DefaultConfig()
	vaultConfig.Address = config.Address

	tlsConfig := &api.TLSConfig{
		CACert:        config.CACert,
		ClientCert:    config.ClientCert,
		ClientKey:     config.ClientKey,
		TLSServerName: config.TLSServerName,
	}
	if err := vaultConfig.ConfigureTLS(tlsConfig); err != nil {
		return nil, err
	}

	return api.NewClient(vaultConfig)
}

// Get token by login and set the value to api.Client.
func (c *clientWrapper) refreshToken(config *VaultEnvelopeConfig, version uint) error {
	c.rwmutex.Lock()
	defer c.rwmutex.Unlock()

	// The token has been refreshed by other goroutine.
	if version < c.version {
		return nil
	}

	var err error
	switch {
	case config.ClientCert != "" && config.ClientKey != "":
		err = c.tlsToken(config)
	case config.RoleId != "":
		err = c.appRoleToken(config)
	default:
		err = fmt.Errorf("invalid authentication configuration %+v", config)
	}

	c.version++
	return err
}

func (c *clientWrapper) tlsToken(config *VaultEnvelopeConfig) error {
	resp, err := c.client.Logical().Write(c.authPath+"cert/login", nil)
	if err != nil {
		return err
	}

	c.client.SetToken(resp.Auth.ClientToken)
	return nil
}

func (c *clientWrapper) appRoleToken(config *VaultEnvelopeConfig) error {
	data := map[string]interface{}{
		"role_id":   config.RoleId,
		"secret_id": config.SecretId,
	}
	resp, err := c.client.Logical().Write(c.authPath+"approle/login", data)
	if err != nil {
		return err
	}

	c.client.SetToken(resp.Auth.ClientToken)
	return nil
}

func (c *clientWrapper) decrypt(keyName string, cipher string) (string, error) {
	var result string

	data := map[string]string{"ciphertext": cipher}
	resp, err := c.request(c.decryptPath+keyName, data)
	if err != nil {
		return result, err
	}

	result, ok := resp.Data["plaintext"].(string)
	if !ok {
		return result, fmt.Errorf("failed type assertion of vault decrypt response to string")
	}

	return result, nil
}

func (c *clientWrapper) encrypt(keyName string, plain string) (string, error) {
	var result string

	data := map[string]string{"plaintext": plain}
	resp, err := c.request(c.encryptPath+keyName, data)
	if err != nil {
		return result, err
	}

	result, ok := resp.Data["ciphertext"].(string)
	if !ok {
		return result, fmt.Errorf("failed type assertion of vault encrypt response to string")
	}

	return result, nil
}

// This request check the response status code. If get code 403, it sets forbidden true.
func (c *clientWrapper) request(path string, data interface{}) (*api.Secret, error) {
	c.rwmutex.RLock()
	defer c.rwmutex.RUnlock()

	req := c.client.NewRequest("POST", path)
	if err := req.SetJSONBody(data); err != nil {
		return nil, err
	}

	resp, err := c.client.RawRequest(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if resp.StatusCode == 403 {
		return nil, &forbiddenError{version: c.version, err: err}
	}
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == 200 {
		secret, err := api.ParseSecret(resp.Body)
		if err != nil {
			return nil, err
		}
		return secret, nil
	}

	return nil, nil
}

// Return this error when get HTTP code 403.
type forbiddenError struct {
	version uint
	err     error
}

func (e *forbiddenError) Error() string {
	return fmt.Sprintf("version %d, error %s", e.version, e.err)
}
