package vault_test

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/vault/api"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/ory/dockertest"
	"github.com/ory/dockertest/docker"
)

func TestVault(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Vault Suite")
}

type vaultConfig struct {
	Role     string
	Mount    string
	Token    string
	URL      *url.URL
	CA       *x509.Certificate
	CertPool *x509.CertPool
}

var (
	pool     *dockertest.Pool
	resource *dockertest.Resource
	waiter   docker.CloseWaiter

	vaultConf, vaultTLSConf, vaultMountConf vaultConfig
	defaultTTL, maxTTL                      time.Duration
)

var _ = BeforeSuite(func() {
	host := "localhost"
	if os.Getenv("DOCKER_HOST") != "" {
		u, err := url.Parse(os.Getenv("DOCKER_HOST"))
		Expect(err).To(Succeed())
		host, _, err = net.SplitHostPort(u.Host)
		Expect(err).To(Succeed())
	}

	log.SetOutput(GinkgoWriter)

	cert, key, err := generateCertAndKey(host, net.IPv4(127, 0, 0, 1))
	Expect(err).To(Succeed())

	pool, err = dockertest.NewPool("")
	Expect(err).To(Succeed())

	pool.MaxWait = time.Second * 10

	By("Starting the Vault container", func() {
		cp := x509.NewCertPool()
		Expect(cp.AppendCertsFromPEM(cert)).To(BeTrue())
		token := "mysecrettoken"
		role := "test"

		repo := "vault"
		version := "1.0.0"
		img := repo + ":" + version
		_, err = pool.Client.InspectImage(img)
		if err != nil {
			// Pull image
			Expect(pool.Client.PullImage(docker.PullImageOptions{
				Repository:   repo,
				Tag:          version,
				OutputStream: GinkgoWriter,
			}, docker.AuthConfiguration{})).To(Succeed())
		}

		defaultTTL = 168 * time.Hour
		maxTTL = 720 * time.Hour
		c, err := pool.Client.CreateContainer(docker.CreateContainerOptions{
			Name: "vault",
			Config: &docker.Config{
				Image: img,
				Env: []string{
					"VAULT_DEV_ROOT_TOKEN_ID=" + token,
					fmt.Sprintf(`VAULT_LOCAL_CONFIG={
						"default_lease_ttl": "%s",
						"max_lease_ttl": "%s",
						"disable_mlock": true,
						"listener": [{
							"tcp" :{
								"address": "0.0.0.0:8201",
								"tls_cert_file": "/vault/file/cert.pem",
								"tls_key_file": "/vault/file/key.pem"
							}
						}]
					}`, defaultTTL, maxTTL),
				},
				ExposedPorts: map[docker.Port]struct{}{
					docker.Port("8200"): struct{}{},
					docker.Port("8201"): struct{}{},
				},
			},
			HostConfig: &docker.HostConfig{
				PublishAllPorts: true,
				PortBindings: map[docker.Port][]docker.PortBinding{
					"8200": []docker.PortBinding{{HostPort: "8200"}},
					"8201": []docker.PortBinding{{HostPort: "8201"}},
				},
			},
		})
		Expect(err).To(Succeed())

		b := &bytes.Buffer{}
		archive := tar.NewWriter(b)
		Expect(archive.WriteHeader(&tar.Header{
			Name: "/cert.pem",
			Mode: 0644,
			Size: int64(len(cert)),
		})).To(Succeed())
		Expect(archive.Write(cert)).To(Equal(len(cert)))
		Expect(archive.WriteHeader(&tar.Header{
			Name: "/key.pem",
			Mode: 0644,
			Size: int64(len(key)),
		})).To(Succeed())
		Expect(archive.Write(key)).To(Equal(len(key)))
		Expect(archive.Close()).To(Succeed())

		Expect(pool.Client.UploadToContainer(c.ID, docker.UploadToContainerOptions{
			InputStream: b,
			Path:        "/vault/file/",
		})).To(Succeed())

		Expect(pool.Client.StartContainer(c.ID, nil)).To(Succeed())

		c, err = pool.Client.InspectContainer(c.ID)
		Expect(err).To(Succeed())

		waiter, err = pool.Client.AttachToContainerNonBlocking(docker.AttachToContainerOptions{
			Container:    c.ID,
			OutputStream: GinkgoWriter,
			ErrorStream:  GinkgoWriter,
			Stderr:       true,
			Stdout:       true,
			Stream:       true,
		})
		Expect(err).To(Succeed())

		resource = &dockertest.Resource{Container: c}

		conf := api.DefaultConfig()
		conf.Address = "http://" + net.JoinHostPort(host, "8200")
		cli, err := api.NewClient(conf)
		Expect(err).To(Succeed())
		cli.SetToken(token)

		// Mount PKI at /pki
		Expect(pool.Retry(func() error {
			_, err := cli.Logical().Read("pki/certs")
			return err
		})).To(Succeed())

		Expect(cli.Sys().Mount("pki", &api.MountInput{
			Type: "pki",
			Config: api.MountConfigInput{
				MaxLeaseTTL: "87600h",
			},
		})).To(Succeed())
		resp, err := cli.Logical().Write("pki/root/generate/internal", map[string]interface{}{
			"ttl":         "87600h",
			"common_name": "my_vault",
			"ip_sans":     c.NetworkSettings.IPAddress,
			"format":      "der",
		})
		Expect(err).To(Succeed())
		caCertDER, err := base64.StdEncoding.DecodeString(resp.Data["certificate"].(string))
		Expect(err).To(Succeed())
		vaultCA, err := x509.ParseCertificate(caCertDER)
		Expect(err).To(Succeed())

		_, err = cli.Logical().Write("pki/roles/"+role, map[string]interface{}{
			"allowed_domains":    "myserver.com",
			"allow_subdomains":   true,
			"allow_any_name":     true,
			"key_type":           "any",
			"allowed_other_sans": "1.3.6.1.4.1.311.20.2.3;utf8:*",
		})
		Expect(err).To(Succeed())

		// Mount pki at /mount-test-pki
		Expect(pool.Retry(func() error {
			_, err := cli.Logical().Read("mount-test-pki/certs")
			return err
		})).To(Succeed())

		Expect(cli.Sys().Mount("mount-test-pki", &api.MountInput{
			Type: "pki",
			Config: api.MountConfigInput{
				MaxLeaseTTL: "87600h",
			},
		})).To(Succeed())
		_, err = cli.Logical().Write("mount-test-pki/root/generate/internal", map[string]interface{}{
			"ttl":         "87600h",
			"common_name": "my_vault",
			"ip_sans":     c.NetworkSettings.IPAddress,
			"format":      "der",
		})
		Expect(err).To(Succeed())

		_, err = cli.Logical().Write("mount-test-pki/roles/"+role, map[string]interface{}{
			"allowed_domains":    "myserver.com",
			"allow_subdomains":   true,
			"allow_any_name":     true,
			"key_type":           "any",
			"allowed_other_sans": "1.3.6.1.4.1.311.20.2.3;utf8:*",
		})
		Expect(err).To(Succeed())

		vaultConf = vaultConfig{
			Token: token,
			Role:  role,
			URL: &url.URL{
				Scheme: "http",
				Host:   net.JoinHostPort(host, "8200"),
			},
		}
		vaultMountConf = vaultConfig{
			Token: token,
			Mount: "mount-test-pki",
			Role:  role,
			URL: &url.URL{
				Scheme: "http",
				Host:   net.JoinHostPort(host, "8200"),
			},
		}
		vaultTLSConf = vaultConfig{
			Token:    token,
			Role:     role,
			CertPool: cp,
			CA:       vaultCA,
			URL: &url.URL{
				Scheme: "https",
				Host:   net.JoinHostPort(host, "8201"),
			},
		}
	})
})

var _ = AfterSuite(func() {
	Expect(waiter.Close()).To(Succeed())
	Expect(waiter.Wait()).To(Succeed())
	Expect(pool.Purge(resource)).To(Succeed())
})

func generateCertAndKey(SAN string, IPSAN net.IP) ([]byte, []byte, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	notBefore := time.Now()
	notAfter := notBefore.Add(time.Hour)
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, nil, err
	}
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "Certify Test Cert",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{SAN},
		IPAddresses:           []net.IP{IPSAN},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, priv.Public(), priv)
	if err != nil {
		return nil, nil, err
	}
	certOut := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: derBytes,
	})
	keyOut := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})

	return certOut, keyOut, nil
}
