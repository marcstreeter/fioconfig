package internal

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/ethereum/go-ethereum/crypto/ecies"
)

var NotModifiedError = errors.New("Config unchanged on server")

type App struct {
	PrivKey         *ecies.PrivateKey
	EncryptedConfig string
	SecretsDir      string

	client    *http.Client
	configUrl string
}

func createClient(sota_config string) (*http.Client, *ecdsa.PrivateKey) {
	certFile := filepath.Join(sota_config, "client.pem")
	keyFile := filepath.Join(sota_config, "pkey.pem")
	caFile := filepath.Join(sota_config, "root.crt")

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Fatal(err)
	}

	caCert, err := ioutil.ReadFile(caFile)
	if err != nil {
		log.Fatal(err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caCertPool,
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	return &http.Client{Timeout: time.Second * 30, Transport: transport}, cert.PrivateKey.(*ecdsa.PrivateKey)
}

func NewApp(sota_config, secrets_dir string, testing bool) (*App, error) {
	var client *http.Client
	var priv *ecdsa.PrivateKey
	if testing {
		path := filepath.Join(sota_config, "pkey.pem")
		pkey_pem, err := ioutil.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("Unable to read private key: %v", err)
		}

		block, _ := pem.Decode(pkey_pem)
		if block == nil {
			return nil, fmt.Errorf("Unable to decode private key(%s): %v", path, err)
		}

		p, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("Unable to parse private key(%s): %v", path, err)
		}
		priv = p.(*ecdsa.PrivateKey)
	} else {
		client, priv = createClient(sota_config)
	}

	url := os.Getenv("CONFIG_URL")
	if len(url) == 0 {
		url = "https://ota-lite.foundries.io:8443/config"
	}
	app := App{
		PrivKey:         ecies.ImportECDSA(priv),
		EncryptedConfig: filepath.Join(sota_config, "config.encrypted"),
		SecretsDir:      secrets_dir,
		client:          client,
		configUrl:       url,
	}

	return &app, nil
}

// Do an atomic update of the file if needed
func updateSecret(secretFile string, newContent []byte) (bool, error) {
	curContent, err := ioutil.ReadFile(secretFile)
	if err == nil && bytes.Equal(newContent, curContent) {
		return false, nil
	}
	tmp := secretFile + ".tmp"
	if err := ioutil.WriteFile(tmp, newContent, 0640); err != nil {
		return true, fmt.Errorf("Unable to create %s: %v", tmp, err)
	}
	if err := os.Rename(tmp, secretFile); err != nil {
		return true, fmt.Errorf("Unable to update secret: %s - %w", secretFile, err)
	}
	return true, nil
}

func (a *App) Extract() error {
	if _, err := os.Stat(a.SecretsDir); err != nil {
		return err
	}
	config, err := Unmarshall(a.PrivKey, a.EncryptedConfig)
	if err != nil {
		return err
	}

	for fname, cfgFile := range config {
		log.Printf("Extracting %s", fname)
		fullpath := filepath.Join(a.SecretsDir, fname)
		changed, err := updateSecret(fullpath, []byte(cfgFile.Value))
		if err != nil {
			return err
		}
		if changed && len(cfgFile.OnChanged) > 0 {
			log.Printf("Running on-change command for %s: %v", fname, cfgFile.OnChanged)
			cmd := exec.Command(cfgFile.OnChanged[0], cfgFile.OnChanged[1:]...)
			cmd.Env = append(os.Environ(), "CONFIG_FILE="+fullpath)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				log.Printf("Unable to run command: %v", err)
			}
		}
	}
	return nil
}

func close(c io.Closer, name string) {
	err := c.Close()
	if err != nil {
		log.Printf("Unexpected error closing %s: %s", name, err)
	}
}

// Do an atomic change to the secrets file so that a reader of the current
// secrets file won't hit race conditions.
func safeWrite(input io.ReadCloser, path string, modtime time.Time) error {
	defer close(input, path)

	safepath := path + ".tmp"
	to, err := os.OpenFile(safepath, os.O_RDWR|os.O_CREATE, 0644)
	defer close(to, safepath)
	if err != nil {
		return fmt.Errorf("Unable to create new secrets: %s - %w", path, err)
	}

	_, err = io.Copy(to, input)
	if err != nil {
		return fmt.Errorf("Unable to copy secrets to: %s - %w", path, err)
	}

	if err := os.Rename(safepath, path); err != nil {
		return fmt.Errorf("Unable to link secrets to: %s - %w", path, err)
	}

	err = os.Chtimes(path, modtime, modtime)
	if err != nil {
		return fmt.Errorf("Unable to set modified time %s - %w", path, err)
	}
	return nil
}

func (a *App) CheckIn() error {
	req, err := http.NewRequest("GET", a.configUrl, nil)
	if err != nil {
		return err
	}

	fi, err := os.Stat(a.EncryptedConfig)
	if err == nil {
		// Don't pull it down unless we need to
		ts := fi.ModTime().UTC().Format(time.RFC1123)
		req.Header.Add("If-Modified-Since", ts)
	}

	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("Unable to get: %s - %v", a.configUrl, err)
	}
	if res.StatusCode == 200 {
		modtime, err := time.Parse(time.RFC1123, res.Header.Get("Date"))
		if err != nil {
			log.Printf("Unable to get modtime of config file, defaulting to 'now': %s", err)
			modtime = time.Now()
		}
		if err := safeWrite(res.Body, a.EncryptedConfig, modtime); err != nil {
			return err
		}
	} else if res.StatusCode == 304 {
		log.Println("Config on server has not changed")
		return NotModifiedError
	} else if res.StatusCode == 204 {
		log.Println("Device has no config defined on server")
		return NotModifiedError
	} else {
		msg, _ := ioutil.ReadAll(res.Body)
		res.Body.Close()
		return fmt.Errorf("Unable to get %s - HTTP_%d: %s", a.configUrl, res.StatusCode, string(msg))
	}
	return a.Extract()
}
