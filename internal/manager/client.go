package manager

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/goquorum/quorum-plugin-hashicorp-account-store/internal/config"
	"github.com/hashicorp/vault/api"
	"github.com/pborman/uuid"
)

// Environment variable name for Hashicorp Vault authentication credential
const (
	DefaultRoleIDEnv   = "QRM_HASHIVLT_ROLE_ID"
	DefaultSecretIDEnv = "QRM_HASHIVLT_SECRET_ID"
	DefaultTokenEnv    = "QRM_HASHIVLT_TOKEN"
)

// vaultClientManager manages all the authenticated clients configured for a particular Vault
// server.  It contains all the clients configured for use, each authenticated using individual auth config.
// vaultClientManager is used for Vault read and write operations.
type vaultClientManager struct {
	vaultAddr     string
	acctConfigDir string
	// map of authenticated clients with keys equal to their corresponding authID
	clients map[string]*authenticatedClient
}

// newVaultClientManager creates a authenticated clients for each auth config provided in the VaultConfig and returns them
// wrapped in a vaultClientManager
func newVaultClientManager(config config.VaultConfig) (*vaultClientManager, error) {
	clients := make(map[string]*authenticatedClient, len(config.Auth))
	for _, auth := range config.Auth {
		client, err := newAuthenticatedClient(config.URL, auth, config.TLS)
		if err != nil {
			return nil, fmt.Errorf("unable to create client for Vault %v using auth %v: err: %v", config.URL, auth.AuthID, err)
		}
		clients[auth.AuthID] = client
	}
	return &vaultClientManager{
		vaultAddr:     config.URL,
		acctConfigDir: config.AccountConfigDir,
		clients:       clients,
	}, nil
}

// GetKey reads the configfile contents of filename, retrieving the defined secret from the Vault
// using the client authenticated using the auth set of credentials
func (m *vaultClientManager) GetKey(addr common.Address, filename string, auth string) (*Key, error) {
	fileBytes, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var conf config.AccountConfig
	if err := json.Unmarshal(fileBytes, &conf); err != nil {
		return nil, err
	}

	if conf == (config.AccountConfig{}) {
		return nil, fmt.Errorf("unable to read vault account config from file %v", filename)
	}

	// Make sure the contents of the file matches the requested key
	if !common.IsHexAddress(conf.Address) {
		return nil, fmt.Errorf("invalid hex address from file contents: %v", addr)
	}

	if confAddr := common.HexToAddress(conf.Address); confAddr != addr {
		return nil, fmt.Errorf("acctconfig file content mismatch: have account %x, want %x", confAddr, addr)
	}

	hexKey, err := m.getSecretFromVault(conf.VaultSecret)
	if err != nil {
		return nil, err
	}

	key, err := crypto.HexToECDSA(hexKey)
	if err != nil {
		return nil, fmt.Errorf("unable to parse data from Hashicorp Vault to *ecdsa.PrivateKey: %v", err)
	}

	return &Key{
		Id:         uuid.UUID(conf.Id),
		Address:    crypto.PubkeyToAddress(key.PublicKey),
		PrivateKey: key,
	}, nil
}

// getSecretFromVault retrieves the secret described by the VaultSecretConfig from the Vault. Expects RLock to be held.
func (m *vaultClientManager) getSecretFromVault(vaultAccountConfig config.VaultSecretConfig) (string, error) {
	client, ok := m.clients[vaultAccountConfig.AuthID]
	if !ok {
		return "", fmt.Errorf("no client configured for Vault %v and authID %v", m.vaultAddr, vaultAccountConfig.AuthID)
	}

	path := fmt.Sprintf("%s/data/%s", vaultAccountConfig.PathParams.SecretEnginePath, vaultAccountConfig.PathParams.SecretPath)

	versionData := make(map[string][]string)
	versionData["version"] = []string{strconv.FormatInt(vaultAccountConfig.PathParams.SecretVersion, 10)}

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

// StoreKey stores the Key in the Vault location defined by the VaultSecretConfig.  The necessary config is written to filename.
// The new account is returned along with the URI of the secret's Vault location.
func (m vaultClientManager) StoreKey(filename string, vaultConfig config.VaultSecretConfig, k *Key) (accounts.Account, string, error) {
	secretUri, secretVersion, err := m.storeInVault(vaultConfig, k)
	if err != nil {
		return accounts.Account{}, "", err
	}

	// include the version of the newly created vault secret in the data written to file
	vaultConfig.PathParams.SecretVersion = secretVersion
	acctConfig := config.AccountConfig{
		Address:     hex.EncodeToString(k.Address[:]),
		VaultSecret: vaultConfig,
		Id:          k.Id.String(),
		Version:     version,
	}

	if err := m.storeInFile(filename, acctConfig, k); err != nil {
		return accounts.Account{}, "", fmt.Errorf("secret written to Vault but unable to write data to file: secret uri: %v, err: %v", secretUri, err)
	}

	acct, err := config.ToAccount(acctConfig, m.vaultAddr)
	if err != nil {
		return accounts.Account{}, "", fmt.Errorf("secret written to Vault but unable to parse as account: secret uri: %v, err: %v", secretUri, err)
	}

	return acct, secretUri, nil
}

// storeInVault stores the Key in the Vault location defined by the VaultSecretConfig.  The URI of the secret's Vault
// location is returned along with the version of the new secret.
func (m vaultClientManager) storeInVault(vaultConfig config.VaultSecretConfig, k *Key) (string, int64, error) {
	client, ok := m.clients[vaultConfig.AuthID]
	if !ok {
		return "", 0, fmt.Errorf("no client configured for Vault %v and authID %v", m.vaultAddr, vaultConfig.AuthID)
	}

	path := fmt.Sprintf("%s/data/%s", vaultConfig.PathParams.SecretEnginePath, vaultConfig.PathParams.SecretPath)

	address := k.Address
	addrHex := hex.EncodeToString(address[:])

	keyBytes := crypto.FromECDSA(k.PrivateKey)
	keyHex := hex.EncodeToString(keyBytes)

	data := make(map[string]interface{})
	data["data"] = map[string]interface{}{
		addrHex: keyHex,
	}

	if !vaultConfig.InsecureSkipCas {
		data["options"] = map[string]interface{}{
			"cas": vaultConfig.CasValue,
		}
	}

	resp, err := client.Logical().Write(path, data)
	if err != nil {
		return "", 0, fmt.Errorf("unable to write secret to Vault: %v", err)
	}

	v, ok := resp.Data["version"]
	if !ok {
		secretUri := fmt.Sprintf("%v/v1/%v", client.Address(), path)
		return "", 0, fmt.Errorf("secret written to Vault but unable to get version: secret uri: %v, err %v", secretUri, err)
	}
	vJson, ok := v.(json.Number)
	secretVersion, err := vJson.Int64()
	if err != nil {
		secretUri := fmt.Sprintf("%v/v1/%v", client.Address(), path)
		return "", 0, fmt.Errorf("secret written to Vault but unable to convert version in Vault response to int64: secret: %v, version: %v", secretUri, vJson.String())
	}

	secretUri := fmt.Sprintf("%v/v1/%v?version=%v", client.Address(), path, secretVersion)
	return secretUri, secretVersion, nil
}

func (m vaultClientManager) storeInFile(filename string, acctConfig config.AccountConfig, k *Key) error {
	toStore, err := json.Marshal(acctConfig)
	if err != nil {
		return err
	}
	// Write into temporary file
	tmpName, err := writeTemporaryKeyFile(filename, toStore)
	if err != nil {
		return err
	}

	return os.Rename(tmpName, filename)
}

// JoinPath joins filename with the acctconfig directory unless it is already absolute.
func (m vaultClientManager) JoinPath(filename string) string {
	if filepath.IsAbs(filename) {
		return filename
	}
	return filepath.Join(m.acctConfigDir, filename)
}

// authenticatedClient contains a Vault Client and Renewer for the client to perform reauthentication of the  client when
// necessary.
type authenticatedClient struct {
	*api.Client
	renewer    *api.Renewer
	authConfig config.VaultAuth
}

// newAuthenticatedClient creates an authenticated Vault client using the credentials provided as environment variables
// (either logging in using the AppRole or using a provided token directly).  Providing tls will configure the client
// to use TLS for Vault communications.  If the AppRole token is renewable the client will be started with a renewer.
func newAuthenticatedClient(vaultAddr string, authConfig config.VaultAuth, tls config.TLS) (*authenticatedClient, error) {
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

	if renewable, _ := resp.TokenIsRenewable(); renewable {
		go ac.renew()
	}

	return ac, nil
}

// approleLogin returns the result of a login request to the Vault using the client and the authCredentials.  If approlePath
// is not provided the default value of approle will be used.
func approleLogin(c *api.Client, creds authCredentials, approlePath string) (*api.Secret, error) {
	body := map[string]interface{}{"role_id": creds.roleID, "secret_id": creds.secretID}

	approle := approlePath
	if approle == "" {
		approle = "approle"
	}

	return c.Logical().Write(fmt.Sprintf("auth/%s/login", approle), body)
}

// getAuthCredentials retrieves the authCredentials set on the environment, returning an error if an invalid combination
// has been set.  If authID is provided, getAuthCredentials will expect each environment variable name to be prefixed with
// "{authID}_".
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

const reauthRetryInterval = 5000 * time.Millisecond

// renew starts the client's background process for renewing the its auth token.  If the renewal fails, renew will attempt
// reauthentication indefinitely.
func (ac *authenticatedClient) renew() {
	go ac.renewer.Renew()

	for {
		select {
		case err := <-ac.renewer.DoneCh():
			// Renewal has stopped either due to an unexpected reason (i.e. some error) or an expected reason
			// (e.g. token TTL exceeded).  Either way we must re-authenticate and get a new token.
			switch err {
			case nil:
				log.Printf("[DEBUG] renewal of Vault auth token failed, attempting re-authentication: auth = %v", ac.authConfig)
			default:
				log.Printf("[DEBUG] renewal of Vault auth token failed, attempting re-authentication: auth = %v, err = %v", ac.authConfig, err)
			}

			for i := 1; ; i++ {
				err := ac.reauthenticate()
				if err == nil {
					log.Printf("[DEBUG] successfully re-authenticated with Vault: auth = %v", ac.authConfig)
					break
				}
				log.Printf("[ERROR] unable to reauthenticate with Vault (attempt %v): auth = %v, err = %v", i, ac.authConfig, err)
				time.Sleep(reauthRetryInterval)
			}
			go ac.renewer.Renew()

		case _ = <-ac.renewer.RenewCh():
			log.Printf("[DEBUG] successfully renewed Vault auth token: auth = %v", ac.authConfig)
		}
	}
}

// reauthenticate re-reads the authentication credentials from the environments, makes the approle login request to the
// Vault, updates the client and resets the renewal process.
func (ac *authenticatedClient) reauthenticate() error {
	creds, err := getAuthCredentials(ac.authConfig.AuthID)
	if err != nil {
		return err
	}

	// authenticate the client using approle
	resp, err := approleLogin(ac.Client, creds, ac.authConfig.ApprolePath)
	if err != nil {
		return err
	}

	t, err := resp.TokenID()
	if err != nil {
		return err
	}
	ac.Client.SetToken(t)

	r, err := ac.Client.NewRenewer(&api.RenewerInput{Secret: resp})
	if err != nil {
		return err
	}
	ac.renewer = r

	return nil
}

type authCredentials struct {
	roleID, secretID, token string
}

func (a authCredentials) usingApproleAuth() bool {
	return a.roleID != "" && a.secretID != ""
}

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

func applyPrefix(pre, val string) string {
	if pre == "" {
		return val
	}

	return fmt.Sprintf("%v_%v", pre, val)
}
