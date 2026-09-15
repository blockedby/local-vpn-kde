package localvpn

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type AssetOptions struct {
	Base, TemplateDir, Endpoint, DNS string
	Port, CertificateDays            int
}

// Assets creates a local CA only when all identity files are absent. Partial
// identities fail closed, and existing identities are reused byte for byte.
func Assets(o AssetOptions) error {
	if o.Endpoint != "127.0.0.1" && o.Endpoint != "localhost" {
		return errors.New("local endpoint required")
	}
	if o.DNS != "8.8.8.8" && o.DNS != "8.8.4.4" {
		return errors.New("unsupported pushed DNS")
	}
	if o.Port < 1 || o.Port > 65535 || o.CertificateDays < 1 || o.CertificateDays > 36500 {
		return errors.New("invalid asset settings")
	}
	base, err := directory(o.Base, true, true)
	if err != nil {
		return err
	}
	fds := []int{base}
	defer func() {
		for i := len(fds) - 1; i >= 0; i-- {
			unix.Close(fds[i])
		}
	}()
	paths := map[string]int{"": base}
	for _, path := range []string{"openvpn", "openvpn/pki", "openvpn/server", "openvpn/client", "rendered", "rendered/openvpn", "rendered/openvpn/pki", "vibe-vpn"} {
		parent := filepath.Dir(path)
		if parent == "." {
			parent = ""
		}
		fd, e := childDirectory(paths[parent], filepath.Base(path), true, true)
		if e != nil {
			return e
		}
		paths[path] = fd
		fds = append(fds, fd)
	}
	// Serialize identity creation independently of optional lifecycle callers.
	lock, err := unix.Openat(base, ".assets.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer unix.Close(lock)
	if _, err = privateInode(lock); err != nil {
		return err
	}
	if err = unix.Flock(lock, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err
	}
	defer unix.Flock(lock, unix.LOCK_UN)
	pki := paths["openvpn/pki"]
	material := map[string][]byte{}
	names := []string{"ca.crt", "ca.key", "server.crt", "server.key", "client.crt", "client.key", "ta.key"}
	present := 0
	for _, name := range names {
		data, e := readAt(pki, name)
		if e != nil && !errors.Is(e, unix.ENOENT) {
			return e
		}
		if e == nil {
			present++
			if len(data) == 0 {
				return errors.New("empty identity file")
			}
			material[name] = data
		}
	}
	if present > 0 && present < len(names) {
		return errors.New("partial PKI: refusing identity rotation")
	}
	if present == 0 {
		material, err = generatePKI(o.CertificateDays)
		if err != nil {
			return err
		}
	}
	templateDir, err := directory(o.TemplateDir, false, false)
	if err != nil {
		return err
	}
	defer unix.Close(templateDir)
	serverTemplate, err := readAt(templateDir, "local-server.tpl")
	if err != nil {
		return err
	}
	clientTemplate, err := readAt(templateDir, "local-client.ovpn.template")
	if err != nil {
		return err
	}
	server := strings.ReplaceAll(string(serverTemplate), "{{OPENVPN_PUSH_DNS}}", o.DNS)
	profile := string(clientTemplate)
	for marker, value := range map[string]string{"LOCAL_REMOTE": o.Endpoint, "LOCAL_PORT": strconv.Itoa(o.Port), "CA_CRT": strings.TrimSpace(string(material["ca.crt"])), "CLIENT_CRT": strings.TrimSpace(string(material["client.crt"])), "CLIENT_KEY": strings.TrimSpace(string(material["client.key"])), "TA_KEY": strings.TrimSpace(string(material["ta.key"]))} {
		profile = strings.ReplaceAll(profile, "{{"+marker+"}}", value)
	}
	if strings.Contains(profile, "{{") || strings.Contains(profile, "}}") || strings.Contains(server, "{{") {
		return errors.New("unresolved template")
	}
	for _, fd := range fds {
		if err = unix.Fchmod(fd, 0700); err != nil {
			return err
		}
		if err = unix.Fsync(fd); err != nil {
			return err
		}
	}
	if present == 0 {
		for _, name := range names {
			if err = atomicWrite(pki, name, material[name]); err != nil {
				return err
			}
		}
	}
	// Existing PKI permissions are hardened through checked descriptors only.
	for _, name := range names {
		fd, e := unix.Openat(pki, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if e != nil {
			return e
		}
		_, e = privateInode(fd)
		if e == nil {
			e = unix.Fchmod(fd, 0600)
		}
		if e == nil {
			e = unix.Fsync(fd)
		}
		unix.Close(fd)
		if e != nil {
			return e
		}
	}
	for _, path := range []string{"openvpn/server", "rendered/openvpn"} {
		if err = atomicWrite(paths[path], "server.conf", []byte(server)); err != nil {
			return err
		}
	}
	if err = atomicWrite(paths["openvpn/client"], "vpnkit-local.ovpn", []byte(strings.TrimRight(profile, "\r\n")+"\n")); err != nil {
		return err
	}
	for _, name := range []string{"ca.crt", "server.crt", "server.key", "ta.key"} {
		if err = atomicWrite(paths["rendered/openvpn/pki"], name, material[name]); err != nil {
			return err
		}
	}
	return nil
}

func generatePKI(days int) (map[string][]byte, error) {
	material := map[string][]byte{}
	expires := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	newCertificate := func(name string) (*x509.Certificate, *rsa.PrivateKey, error) {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return nil, nil, err
		}
		serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
		if err != nil {
			return nil, nil, err
		}
		if serial.Sign() == 0 {
			serial.SetInt64(1)
		}
		return &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-5 * time.Minute), NotAfter: expires, BasicConstraintsValid: true}, key, nil
	}
	ca, key, err := newCertificate("vpnkit-local-ca")
	if err != nil {
		return nil, err
	}
	ca.IsCA = true
	ca.MaxPathLen = 1
	ca.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	write := func(name string, cert, parent *x509.Certificate, subjectKey, signer *rsa.PrivateKey) error {
		der, err := x509.CreateCertificate(rand.Reader, cert, parent, &subjectKey.PublicKey, signer)
		if err != nil {
			return err
		}
		material[name+".crt"] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		material[name+".key"] = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(subjectKey)})
		return nil
	}
	if err = write("ca", ca, ca, key, key); err != nil {
		return nil, err
	}
	for _, spec := range []struct {
		name, cn string
		usage    x509.ExtKeyUsage
	}{{"server", "vpnkit-local-server", x509.ExtKeyUsageServerAuth}, {"client", "vpnkit-local", x509.ExtKeyUsageClientAuth}} {
		cert, subjectKey, e := newCertificate(spec.cn)
		if e != nil {
			return nil, e
		}
		cert.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
		cert.ExtKeyUsage = []x509.ExtKeyUsage{spec.usage}
		if e = write(spec.name, cert, ca, subjectKey, key); e != nil {
			return nil, e
		}
	}
	var random [256]byte
	if _, err = rand.Read(random[:]); err != nil {
		return nil, err
	}
	text := "#\n# 2048 bit OpenVPN static key\n#\n-----BEGIN OpenVPN Static key V1-----\n"
	encoded := hex.EncodeToString(random[:])
	for len(encoded) > 0 {
		text += encoded[:32] + "\n"
		encoded = encoded[32:]
	}
	text += "-----END OpenVPN Static key V1-----\n"
	material["ta.key"] = []byte(text)
	return material, nil
}
