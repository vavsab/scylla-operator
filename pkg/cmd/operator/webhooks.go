package operator

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/scylladb/scylla-operator/pkg/admissionreview"
	scyllav1 "github.com/scylladb/scylla-operator/pkg/api/scylla/v1"
	"github.com/scylladb/scylla-operator/pkg/api/validation"
	"github.com/scylladb/scylla-operator/pkg/cmdutil"
	"github.com/scylladb/scylla-operator/pkg/genericclioptions"
	"github.com/scylladb/scylla-operator/pkg/signals"
	"github.com/scylladb/scylla-operator/pkg/version"
	"github.com/spf13/cobra"
	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
)

type WebhookOptions struct {
	TLSCertFile, TLSKeyFile        string
	Port                           int
	InsecureGenerateLocalhostCerts bool

	servingCertificate tls.Certificate
}

func NewWebhookOptions(streams genericclioptions.IOStreams) *WebhookOptions {
	return &WebhookOptions{
		Port: 5000,
	}
}

func NewWebhookCmd(streams genericclioptions.IOStreams) *cobra.Command {
	o := NewWebhookOptions(streams)

	cmd := &cobra.Command{
		Use:   "run-webhook-server",
		Short: "Run webhook server.",
		Long:  "Run webhook server.",
		RunE: func(cmd *cobra.Command, args []string) error {
			err := o.Validate()
			if err != nil {
				return err
			}

			err = o.Complete()
			if err != nil {
				return err
			}

			err = o.Run(streams, cmd.Name())
			if err != nil {
				return err
			}

			return nil
		},

		SilenceErrors: true,
		SilenceUsage:  true,
	}

	cmd.Flags().StringVarP(&o.TLSCertFile, "tls-cert-file", "", o.TLSCertFile, "File containing the default x509 Certificate for HTTPS. (CA cert, if any, concatenated after server cert).")
	cmd.Flags().StringVarP(&o.TLSKeyFile, "tls-private-key-file", "", o.TLSKeyFile, "File containing the default x509 private key for matching cert file.")
	cmd.Flags().IntVarP(&o.Port, "port", "", o.Port, "Secure port that the webhook listens on.")

	cmd.Flags().BoolVarP(&o.InsecureGenerateLocalhostCerts, "insecure-generate-localhost-cert", "", o.InsecureGenerateLocalhostCerts, "This will automatically generate self-signed certificate valid for localhost. Do not use this in production!")
	return cmd
}

func (o *WebhookOptions) Validate() error {
	var errs []error

	if len(o.TLSCertFile) == 0 && !o.InsecureGenerateLocalhostCerts {
		return errors.New("tls-cert-file can't be empty if tls-private-key-file is set")
	}

	if len(o.TLSKeyFile) == 0 && !o.InsecureGenerateLocalhostCerts {
		return errors.New("tls-private-key-file can't be empty if tls-cert-file is set")
	}

	if o.Port == 0 {
		return errors.New("port can't be zero")
	}

	return utilerrors.NewAggregate(errs)
}

func (o *WebhookOptions) Complete() error {
	var err error

	if o.InsecureGenerateLocalhostCerts {
		klog.Warningf("Generating temporary TLS certificate.")

		privateKey, err := rsa.GenerateKey(rand.Reader, 4096)
		if err != nil {
			return err
		}

		o.servingCertificate.PrivateKey = privateKey

		serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
		serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
		if err != nil {
			return err
		}

		now := time.Now()

		template := x509.Certificate{
			SerialNumber: serialNumber,
			NotBefore:    now,
			NotAfter:     now.Add(24 * time.Hour),

			KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			BasicConstraintsValid: true,
			IsCA:                  true,

			DNSNames: []string{"localhost"},
		}
		derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, privateKey.Public(), privateKey)
		if err != nil {
			return err
		}

		o.servingCertificate.Certificate = [][]byte{derBytes}

	} else {
		o.servingCertificate, err = tls.LoadX509KeyPair(o.TLSCertFile, o.TLSKeyFile)
		if err != nil {
			return err
		}
	}

	return nil
}

func (o *WebhookOptions) Run(streams genericclioptions.IOStreams, commandName string) error {
	klog.Infof("%s version %s", commandName, version.Get())
	klog.Infof("loglevel is set to %q", cmdutil.GetLoglevel())

	var wg sync.WaitGroup
	defer wg.Wait()

	stopCh := signals.StopChannel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-stopCh
		cancel()
	}()

	handler := http.NewServeMux()
	handler.HandleFunc("/readyz", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, err := w.Write([]byte("ok"))
		if err != nil {
			klog.Error(err)
		}
	})
	handler.Handle("/validate", admissionreview.NewHandler(validate))

	server := http.Server{
		Handler: handler,
		Addr:    fmt.Sprintf(":%d", o.Port),
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{o.servingCertificate},
		},
	}

	wg.Add(1)
	go func() {
		defer wg.Done()

		<-ctx.Done()
		klog.Infof("Shutting down the server.")
		err := server.Shutdown(context.Background())
		if err != nil {
			klog.ErrorS(err, "can't shutdown the server")
		}
	}()

	klog.Infof("Starting HTTPS server on address %q.", server.Addr)
	err := server.ListenAndServeTLS("", "")
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func validate(ar *admissionv1.AdmissionReview) error {
	gvr := schema.GroupVersionResource{
		Group:    ar.Request.Resource.Group,
		Version:  ar.Request.Resource.Version,
		Resource: ar.Request.Resource.Resource,
	}

	deserializer := codecs.UniversalDeserializer()

	var err error
	var obj, oldObj runtime.Object
	if ar.Request.Object.Raw != nil {
		obj, _, err = deserializer.Decode(ar.Request.Object.Raw, nil, nil)
		if err != nil {
			return fmt.Errorf("can't decode object %q: %w", gvr, err)
		}
	}
	if ar.Request.OldObject.Raw != nil {
		oldObj, _, err = deserializer.Decode(ar.Request.OldObject.Raw, nil, nil)
		if err != nil {
			return fmt.Errorf("can't decode old object %q: %w", gvr, err)
		}
	}

	switch gvr {
	case scyllav1.GroupVersion.WithResource("scyllaclusters"):
		var errList field.ErrorList
		switch ar.Request.Operation {
		case admissionv1.Create:
			errList = validation.ValidateScyllaCluster(obj.(*scyllav1.ScyllaCluster))
		case admissionv1.Update:
			errList = validation.ValidateScyllaClusterUpdate(obj.(*scyllav1.ScyllaCluster), oldObj.(*scyllav1.ScyllaCluster))
		}

		if len(errList) > 0 {
			return apierrors.NewInvalid(obj.(*scyllav1.ScyllaCluster).GroupVersionKind().GroupKind(), obj.(*scyllav1.ScyllaCluster).Name, errList)
		}
		return nil
	default:
		return fmt.Errorf("unsupported GVR %q", gvr)
	}
}
