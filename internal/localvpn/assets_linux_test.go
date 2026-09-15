package localvpn

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

func TestAssetsIdentityAndReuse(t *testing.T) {
	templates, err := filepath.Abs("../../config/openvpn")
	if err != nil {
		t.Fatal(err)
	}
	o := AssetOptions{Base: t.TempDir(), TemplateDir: templates, Endpoint: "127.0.0.1", Port: 21194, DNS: "8.8.8.8", CertificateDays: 825}
	if err = Assets(o); err != nil {
		t.Fatal(err)
	}
	cert := func(name string) *x509.Certificate {
		t.Helper()
		data, e := os.ReadFile(filepath.Join(o.Base, "openvpn/pki/"+name+".crt"))
		if e != nil {
			t.Fatal(e)
		}
		block, _ := pem.Decode(data)
		if block == nil {
			t.Fatal("invalid certificate PEM")
		}
		parsed, e := x509.ParseCertificate(block.Bytes)
		if e != nil {
			t.Fatal(e)
		}
		return parsed
	}
	ca := cert("ca")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	for _, test := range []struct {
		name  string
		usage x509.ExtKeyUsage
	}{{"server", x509.ExtKeyUsageServerAuth}, {"client", x509.ExtKeyUsageClientAuth}} {
		if _, err = cert(test.name).Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{test.usage}}); err != nil {
			t.Fatal(err)
		}
	}
	identity := filepath.Join(o.Base, "openvpn/pki/ca.key")
	before, err := os.ReadFile(identity)
	if err != nil {
		t.Fatal(err)
	}
	o.Port = 31194
	if err = Assets(o); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(identity)
	if err != nil || string(before) != string(after) {
		t.Fatal("existing identity rotated")
	}
	if err = os.Remove(filepath.Join(o.Base, "openvpn/pki/client.key")); err != nil {
		t.Fatal(err)
	}
	if err = Assets(o); err == nil {
		t.Fatal("partial PKI accepted")
	}
	after, err = os.ReadFile(identity)
	if err != nil || string(before) != string(after) {
		t.Fatal("partial identity modified")
	}
}
