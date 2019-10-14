package hashicorp

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/hashicorp/vault/api"
	"github.com/pborman/uuid"
	"io/ioutil"
	"log"
	"os"
	"strconv"
)

// Environment variable name for Hashicorp Vault authentication credential
const (
	DefaultRoleIDEnv   = "QRM_HASHIVLT_ROLE_ID"
	DefaultSecretIDEnv = "QRM_HASHIVLT_SECRET_ID"
	DefaultTokenEnv    = "QRM_HASHIVLT_TOKEN"
)

type noHashicorpEnvSetErr struct {
	roleIdEnv, secretIdEnv, tokenEnv string
}

func (e noHashicorpEnvSetErr) Error() string {
	return fmt.Sprintf("environment variables are necessary to authenticate with Hashicorp Vault: set %v and %v if using Approle authentication, else set %v", e.roleIdEnv, e.secretIdEnv, e.tokenEnv)
}

type invalidApproleAuthErr struct {
	roleIdEnv, secretIdEnv string
}

func (e invalidApproleAuthErr) Error() string {
	return fmt.Sprintf("both %v and %v environment variables must be set if using Approle authentication", e.roleIdEnv, e.secretIdEnv)
}

type authenticatedClient struct {
	*api.Client
	renewer    *api.Renewer
	authConfig VaultAuth
}

func newAuthenticatedClient(vaultAddr string, authConfig VaultAuth, tls TLS) (*authenticatedClient, error) {
	conf := api.DefaultConfig()
	conf.Address = vaultAddr

	tlsConfig := &api.TLSConfig{
		CACert:     tls.CaCert,
		ClientCert: tls.ClientCert,
		ClientKey:  tls.ClientKey,
	}

	if err := conf.ConfigureTLS(tlsConfig); err != nil {
		return nil, fmt.Errorf("error creating Hashicorp client: %v", err)
	}

	c, err := api.NewClient(conf)
	if err != nil {
		return nil, fmt.Errorf("error creating Hashicorp client: %v", err)
	}

	creds, err := getAuthCredentials(authConfig.AuthID)
	if err != nil {
		return nil, err
	}

	if !creds.usingApproleAuth() {
		// authenticate the client with the token provided
		c.SetToken(creds.token)
		return &authenticatedClient{Client: c}, nil
	}

	// authenticate the client using approle
	resp, err := approleLogin(c, creds, authConfig.ApprolePath)
	if err != nil {
		return nil, err
	}

	t, err := resp.TokenID()
	if err != nil {
		return nil, err
	}
	c.SetToken(t)

	r, err := c.NewRenewer(&api.RenewerInput{Secret: resp})
	if err != nil {
		return nil, err
	}

	ac := &authenticatedClient{Client: c, renewer: r, authConfig: authConfig}
	go ac.renew()

	return ac, nil
}

func approleLogin(c *api.Client, creds authCredentials, approlePath string) (*api.Secret, error) {
	body := map[string]interface{}{"role_id": creds.roleID, "secret_id": creds.secretID}

	approle := approlePath
	if approle == "" {
		approle = "approle"
	}

	return c.Logical().Write(fmt.Sprintf("auth/%s/login", approle), body)
}

type authCredentials struct {
	roleID, secretID, token string
}

func (a authCredentials) usingApproleAuth() bool {
	return a.roleID != "" && a.secretID != ""
}

func getAuthCredentials(authID string) (authCredentials, error) {
	roleIDEnv := applyPrefix(authID, DefaultRoleIDEnv)
	secretIDEnv := applyPrefix(authID, DefaultSecretIDEnv)
	tokenEnv := applyPrefix(authID, DefaultTokenEnv)

	roleID := os.Getenv(roleIDEnv)
	secretID := os.Getenv(secretIDEnv)
	token := os.Getenv(tokenEnv)

	if roleID == "" && secretID == "" && token == "" {
		return authCredentials{}, noHashicorpEnvSetErr{roleIdEnv: roleIDEnv, secretIdEnv: secretIDEnv, tokenEnv: tokenEnv}
	}

	if roleID == "" && secretID != "" || roleID != "" && secretID == "" {
		return authCredentials{}, invalidApproleAuthErr{roleIdEnv: roleIDEnv, secretIdEnv: secretIDEnv}
	}

	return authCredentials{
		roleID:   roleID,
		secretID: secretID,
		token:    token,
	}, nil
}

func (ac *authenticatedClient) renew() {
	go ac.renewer.Renew()

	for {
		select {
		case err := <-ac.renewer.DoneCh():
			// Renewal has stopped either due to an unexpected reason (i.e. some error) or an expected reason
			// (e.g. token TTL exceeded).  Either way we must re-authenticate and get a new token.
			if err != nil {
				log.Println("[DEBUG] renewal of Vault auth token failed, attempting re-authentication: ", err)
			}

			// TODO what to do if re-authentication fails?  wait some time and retry?
			creds, err := getAuthCredentials(ac.authConfig.AuthID)
			if err != nil {
				log.Println("[ERROR] Vault re-authentication failed: ", err)
			}

			// authenticate the client using approle
			resp, err := approleLogin(ac.Client, creds, ac.authConfig.ApprolePath)
			if err != nil {
				log.Println("[ERROR] Vault re-authentication failed: ", err)
			}

			t, err := resp.TokenID()
			if err != nil {
				log.Println("[ERROR] Vault re-authentication failed: ", err)
			}
			ac.Client.SetToken(t)
			go ac.renewer.Renew()

		case renewal := <-ac.renewer.RenewCh():
			log.Printf("[DEBUG] Successfully renewed Vault auth token: %#v", renewal)
		}
	}
}

func applyPrefix(pre, val string) string {
	if pre == "" {
		return val
	}

	return fmt.Sprintf("%v_%v", pre, val)
}

type vaultClientManager struct {
	vaultAddr string
	clients   map[string]*authenticatedClient
}

func newVaultClientManager(config VaultConfig) (*vaultClientManager, error) {
	clients := make(map[string]*authenticatedClient, len(config.Auth))
	for _, auth := range config.Auth {
		client, err := newAuthenticatedClient(config.Addr, auth, config.TLS)
		if err != nil {
			return nil, fmt.Errorf("unable to create client for Vault %v using auth %v: err: %v", config.Addr, auth.AuthID, err)
		}
		clients[auth.AuthID] = client
	}
	return &vaultClientManager{
		vaultAddr: config.Addr,
		clients:   clients,
	}, nil
}

func (m *vaultClientManager) GetKey(addr common.Address, filename string, auth string) (*Key, error) {
	fileBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var config AccountConfig
	if err := json.Unmarshal(fileBytes, &config); err != nil {
		return nil, err
	}

	if config == (AccountConfig{}) {
		return nil, fmt.Errorf("unable to read vault account config from file %v", filename)
	}

	hexKey, err := m.getSecretFromVault(config)
	if err != nil {
		return nil, err
	}

	key, err := crypto.HexToECDSA(hexKey)
	if err != nil {
		return nil, fmt.Errorf("unable to parse data from Hashicorp Vault to *ecdsa.PrivateKey: %v", err)
	}

	return &Key{
		Id:         uuid.UUID(config.Id),
		Address:    crypto.PubkeyToAddress(key.PublicKey),
		PrivateKey: key,
	}, nil
}

// getSecretFromVault retrieves a particular version of the secret 'name' from the provided secret engine. Expects RLock to be held.
func (m *vaultClientManager) getSecretFromVault(config AccountConfig) (string, error) {
	path := fmt.Sprintf("%s/data/%s", config.HashicorpVault.PathParams.SecretEnginePath, config.HashicorpVault.PathParams.SecretPath)

	versionData := make(map[string][]string)
	versionData["version"] = []string{strconv.FormatInt(config.HashicorpVault.PathParams.SecretVersion, 10)}

	client, ok := m.clients[config.HashicorpVault.AuthID]
	if !ok {
		return "", fmt.Errorf("no client configured for Vault %v and authID %v", m.vaultAddr, config.HashicorpVault.AuthID)
	}
	resp, err := client.Logical().ReadWithData(path, versionData)
	if err != nil {
		return "", fmt.Errorf("unable to get secret from Hashicorp Vault: %v", err)
	}
	if resp == nil {
		return "", fmt.Errorf("no data for secret in Hashicorp Vault")
	}

	respData, ok := resp.Data["data"].(map[string]interface{})
	if !ok {
		return "", errors.New("Hashicorp Vault response does not contain data")
	}
	if len(respData) != 1 {
		return "", errors.New("only one key/value pair is allowed in each Hashicorp Vault secret")
	}

	// get secret regardless of key in map
	var s interface{}
	for _, d := range respData {
		s = d
	}
	secret, ok := s.(string)
	if !ok {
		return "", errors.New("Hashicorp Vault response data is not in string format")
	}

	return secret, nil
}

func (m vaultClientManager) StoreKey(filename string, k *Key, auth string) error {
	panic("implement me")
}

func (m vaultClientManager) JoinPath(filename string) string {
	panic("implement me")
}
