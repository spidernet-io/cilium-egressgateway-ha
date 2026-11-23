package webhook

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	certValidity = 10 * 365 * 24 * time.Hour
	// Certificates expiring within this window are regenerated at startup.
	renewBefore = 90 * 24 * time.Hour
)

// CertConfig describes where the self-signed webhook certificate is stored and used.
type CertConfig struct {
	Namespace   string
	SecretName  string
	ServiceName string
	// Webhook configurations whose caBundle is set.
	MutatingWebhookConfiguration   string
	ValidatingWebhookConfiguration string
}

func (c CertConfig) dnsNames() []string {
	svc := c.ServiceName + "." + c.Namespace
	return []string{c.ServiceName, svc, svc + ".svc", svc + ".svc.cluster.local"}
}

// EnsureCert makes sure the Secret holds a valid certificate for the webhook
// Service, sets the caBundle of the webhook configurations and returns the
// certificate to serve. Replicas racing on startup end up with the same Secret.
// The certificate is served from memory: it does not change while the process
// runs, so no file watcher (inotify) is needed.
func EnsureCert(ctx context.Context, c client.Client, cfg CertConfig) (*tls.Certificate, error) {
	log := logf.FromContext(ctx).WithName("webhook-cert")
	key := client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.SecretName}
	var data map[string][]byte
	for attempt := 0; data == nil; attempt++ {
		if attempt == 5 {
			return nil, errors.New("too many conflicts while storing the webhook certificate")
		}
		sec := &corev1.Secret{}
		err := c.Get(ctx, key, sec)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get Secret %s: %w", key, err)
		}
		if err == nil && validCert(sec.Data, cfg.dnsNames(), time.Now()) == nil {
			data = sec.Data
			break
		}
		gen, genErr := generateCert(cfg.dnsNames(), time.Now())
		if genErr != nil {
			return nil, genErr
		}
		if apierrors.IsNotFound(err) {
			sec = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.SecretName},
				Type:       corev1.SecretTypeTLS,
				Data:       gen,
			}
			err = c.Create(ctx, sec)
		} else {
			sec.Data = gen
			err = c.Update(ctx, sec)
		}
		switch {
		case err == nil:
			log.Info("generated the webhook certificate", "secret", key)
			data = gen
		case apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err):
			// Another replica stored one first: use it.
		default:
			return nil, fmt.Errorf("store Secret %s: %w", key, err)
		}
	}

	pair, err := tls.X509KeyPair(data[corev1.TLSCertKey], data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return nil, err
	}
	if err := patchCABundle(ctx, c, cfg, data["ca.crt"]); err != nil {
		return nil, err
	}
	return &pair, nil
}

func patchCABundle(ctx context.Context, c client.Client, cfg CertConfig, ca []byte) error {
	if name := cfg.MutatingWebhookConfiguration; name != "" {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			m := &admissionregistrationv1.MutatingWebhookConfiguration{}
			if err := c.Get(ctx, client.ObjectKey{Name: name}, m); err != nil {
				return err
			}
			changed := false
			for i := range m.Webhooks {
				if !bytes.Equal(m.Webhooks[i].ClientConfig.CABundle, ca) {
					m.Webhooks[i].ClientConfig.CABundle, changed = ca, true
				}
			}
			if !changed {
				return nil
			}
			return c.Update(ctx, m)
		}); err != nil {
			return fmt.Errorf("set caBundle of MutatingWebhookConfiguration %s: %w", name, err)
		}
	}
	if name := cfg.ValidatingWebhookConfiguration; name != "" {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			v := &admissionregistrationv1.ValidatingWebhookConfiguration{}
			if err := c.Get(ctx, client.ObjectKey{Name: name}, v); err != nil {
				return err
			}
			changed := false
			for i := range v.Webhooks {
				if !bytes.Equal(v.Webhooks[i].ClientConfig.CABundle, ca) {
					v.Webhooks[i].ClientConfig.CABundle, changed = ca, true
				}
			}
			if !changed {
				return nil
			}
			return c.Update(ctx, v)
		}); err != nil {
			return fmt.Errorf("set caBundle of ValidatingWebhookConfiguration %s: %w", name, err)
		}
	}
	return nil
}

// validCert checks that data holds a CA and a server certificate signed by it,
// valid for dnsNames and not expiring soon.
func validCert(data map[string][]byte, dnsNames []string, now time.Time) error {
	pair, err := tls.X509KeyPair(data[corev1.TLSCertKey], data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data["ca.crt"]) {
		return errors.New("invalid ca.crt")
	}
	for _, name := range dnsNames {
		if !slices.Contains(leaf.DNSNames, name) {
			return fmt.Errorf("certificate does not cover %s", name)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now.Add(renewBefore),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return err
	}
	return nil
}

func generateCert(dnsNames []string, now time.Time) (map[string][]byte, error) {
	serial := func() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127)) }
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	caSerial, err := serial()
	if err != nil {
		return nil, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          caSerial,
		Subject:               pkix.Name{CommonName: "egressgateway-ha-webhook-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	leafSerial, err := serial()
	if err != nil {
		return nil, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: leafSerial,
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(certValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		"ca.crt":                pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}
