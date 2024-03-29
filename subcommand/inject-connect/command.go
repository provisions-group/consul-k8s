package subcommand

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/consul-k8s/connect-inject"
	"github.com/hashicorp/consul-k8s/helper/cert"
	"github.com/hashicorp/consul/command/flags"
	"github.com/hashicorp/go-hclog"
	"github.com/mitchellh/cli"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type Command struct {
	UI cli.Ui

	flagListen          string
	flagAutoName        string // MutatingWebhookConfiguration for updating
	flagAutoHosts       string // SANs for the auto-generated TLS cert.
	flagCertFile        string // TLS cert for listening (PEM)
	flagKeyFile         string // TLS cert private key (PEM)
	flagDefaultInject   bool   // True to inject by default
	flagConsulImage     string // Docker image for Consul
	flagEnvoyImage      string // Docker image for Envoy
	flagACLAuthMethod   string // Auth Method to use for ACLs, if enabled
	flagCentralConfig   bool   // True to enable central config injection
	flagDefaultProtocol string // Default protocol for use with central config
	flagCASecretName	string // Secret name for Consul CA
	flagCACert			string // Consul CA for auth
	flagTLSServerName	string // SNI hostname for Consul auth
	flagConsulHTTPSSL	bool   // Enable TLS for Consul http
	flagConsulGRPCSSL	bool   // Enable TLS for Consul grpc
	flagSet             *flag.FlagSet

	once sync.Once
	help string
	cert atomic.Value
}

func (c *Command) init() {
	c.flagSet = flag.NewFlagSet("", flag.ContinueOnError)
	c.flagSet.BoolVar(&c.flagDefaultInject, "default-inject", true, "Inject by default.")
	c.flagSet.StringVar(&c.flagListen, "listen", ":8080", "Address to bind listener to.")
	c.flagSet.StringVar(&c.flagAutoName, "tls-auto", "",
		"MutatingWebhookConfiguration name. If specified, will auto generate cert bundle.")
	c.flagSet.StringVar(&c.flagAutoHosts, "tls-auto-hosts", "",
		"Comma-separated hosts for auto-generated TLS cert. If specified, will auto generate cert bundle.")
	c.flagSet.StringVar(&c.flagCertFile, "tls-cert-file", "",
		"PEM-encoded TLS certificate to serve. If blank, will generate random cert.")
	c.flagSet.StringVar(&c.flagKeyFile, "tls-key-file", "",
		"PEM-encoded TLS private key to serve. If blank, will generate random cert.")
	c.flagSet.StringVar(&c.flagConsulImage, "consul-image", connectinject.DefaultConsulImage,
		"Docker image for Consul. Defaults to an Consul 1.3.0.")
	c.flagSet.StringVar(&c.flagEnvoyImage, "envoy-image", connectinject.DefaultEnvoyImage,
		"Docker image for Envoy. Defaults to Envoy 1.8.0.")
	c.flagSet.StringVar(&c.flagACLAuthMethod, "acl-auth-method", "",
		"The name of the Kubernetes Auth Method to use for connectInjection if ACLs are enabled.")
	c.flagSet.BoolVar(&c.flagCentralConfig, "enable-central-config", false, "Enable central config.")
	c.flagSet.StringVar(&c.flagDefaultProtocol, "default-protocol", "",
		"The default protocol to use in central config registrations.")
	c.flagSet.StringVar(&c.flagCASecretName, "consul-cacert-secret", "",
		"Kubernetes secret name for the CA certificate to use for Consul communication.")
	c.flagSet.StringVar(&c.flagCACert, "consul-cacert", "",
		"CA certificate name to use for Consul communication.")
	c.flagSet.StringVar(&c.flagTLSServerName, "consul-tls-server-name", "",
		"SNI hostname to use for Consul communication over TLS.")
	c.flagSet.BoolVar(&c.flagConsulHTTPSSL, "consul-http-ssl", false,
		"Whether or not to enable TLS for Consul communication over HTTP.")
	c.flagSet.BoolVar(&c.flagConsulGRPCSSL, "consul-grpc-ssl", false,
		"Whether or not to enable TLS for Consul communication over GRPC.")
	c.help = flags.Usage(help, c.flagSet)
}

func (c *Command) Run(args []string) int {
	c.once.Do(c.init)
	if err := c.flagSet.Parse(args); err != nil {
		return 1
	}

	// We must have an in-cluster K8S client
	config, err := rest.InClusterConfig()
	if err != nil {
		c.UI.Error(fmt.Sprintf("Error loading in-cluster K8S config: %s", err))
		return 1
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		c.UI.Error(fmt.Sprintf("Error creating K8S client: %s", err))
		return 1
	}

	// Determine where to source the certificates from
	var certSource cert.Source = &cert.GenSource{
		Name:  "Connect Inject",
		Hosts: strings.Split(c.flagAutoHosts, ","),
	}
	if c.flagCertFile != "" {
		certSource = &cert.DiskSource{
			CertPath: c.flagCertFile,
			KeyPath:  c.flagKeyFile,
		}
	}

	// Create the certificate notifier so we can update for certificates,
	// then start all the background routines for updating certificates.
	certCh := make(chan cert.Bundle)
	certNotify := &cert.Notify{Ch: certCh, Source: certSource}
	defer certNotify.Stop()
	go certNotify.Start(context.Background())
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	go c.certWatcher(ctx, certCh, clientset)

	// Build the HTTP handler and server
	injector := connectinject.Handler{
		ImageConsul:         c.flagConsulImage,
		ImageEnvoy:          c.flagEnvoyImage,
		RequireAnnotation:   !c.flagDefaultInject,
		AuthMethod:          c.flagACLAuthMethod,
		CentralConfig:       c.flagCentralConfig,
		DefaultProtocol:     c.flagDefaultProtocol,
		ConsulCASecretName:	 c.flagCASecretName,
		ConsulCACert:        c.flagCACert,
		ConsulTLSServerName: c.flagTLSServerName,
		ConsulHTTPSSL:       c.flagConsulHTTPSSL,
		ConsulGRPCSSL:       c.flagConsulGRPCSSL,
		Log:                 hclog.Default().Named("handler"),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", injector.Handle)
	mux.HandleFunc("/health/ready", c.handleReady)
	var handler http.Handler = mux
	server := &http.Server{
		Addr:      c.flagListen,
		Handler:   handler,
		TLSConfig: &tls.Config{GetCertificate: c.getCertificate},
	}

	c.UI.Info(fmt.Sprintf("Listening on %q...", c.flagListen))
	if err := server.ListenAndServeTLS("", ""); err != nil {
		c.UI.Error(fmt.Sprintf("Error listening: %s", err))
		return 1
	}

	return 0
}

func (c *Command) handleReady(rw http.ResponseWriter, req *http.Request) {
	// Always ready at this point. The main readiness check is whether
	// there is a TLS certificate. If we reached this point it means we
	// served a TLS certificate.
	rw.WriteHeader(204)
}

func (c *Command) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	certRaw := c.cert.Load()
	if certRaw == nil {
		return nil, fmt.Errorf("No certificate available.")
	}

	return certRaw.(*tls.Certificate), nil
}

func (c *Command) certWatcher(ctx context.Context, ch <-chan cert.Bundle, clientset *kubernetes.Clientset) {
	var bundle cert.Bundle
	for {
		select {
		case bundle = <-ch:
			c.UI.Output("Updated certificate bundle received. Updating certs...")
			// Bundle is updated, set it up

		case <-time.After(1 * time.Second):
			// This forces the mutating webhook config to remain updated
			// fairly quickly. This is a jank way to do this and we should
			// look to improve it in the future. Since we use Patch requests
			// it is pretty cheap to do, though.

		case <-ctx.Done():
			// Quit
			return
		}

		cert, err := tls.X509KeyPair(bundle.Cert, bundle.Key)
		if err != nil {
			c.UI.Error(fmt.Sprintf("Error loading TLS keypair: %s", err))
			continue
		}

		// If there is a MWC name set, then update the CA bundle.
		if c.flagAutoName != "" && len(bundle.CACert) > 0 {
			// The CA Bundle value must be base64 encoded
			value := base64.StdEncoding.EncodeToString(bundle.CACert)

			_, err := clientset.Admissionregistration().
				MutatingWebhookConfigurations().
				Patch(c.flagAutoName, types.JSONPatchType, []byte(fmt.Sprintf(
					`[{
						"op": "add",
						"path": "/webhooks/0/clientConfig/caBundle",
						"value": %q
					}]`, value)))
			if err != nil {
				c.UI.Error(fmt.Sprintf(
					"Error updating MutatingWebhookConfiguration: %s",
					err))
				continue
			}
		}

		// Update the certificate
		c.cert.Store(&cert)
	}
}

func (c *Command) Synopsis() string { return synopsis }
func (c *Command) Help() string {
	c.once.Do(c.init)
	return c.help
}

const synopsis = "Inject Connect proxy sidecar."
const help = `
Usage: consul-k8s inject-connect [options]

  Run the admission webhook server for injecting the Consul Connect
  proxy sidecar. The sidecar uses Envoy by default.

`
