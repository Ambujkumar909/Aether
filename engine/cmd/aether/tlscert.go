package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// Self-signed certificates for running on a local network.
//
// This exists because of a browser rule that is easy to discover the hard way:
// getUserMedia is only available in a SECURE CONTEXT. Browsers make one
// exception, for localhost, and none at all for a LAN address. So a colleague
// opening http://192.168.1.20:8443 sees the page load perfectly and then gets
// no camera and no microphone, with an error that does not obviously say why.
//
// There is no configuration that avoids this. Serving over TLS is the only
// option, and on a private network there is no certificate authority to ask,
// so the certificate is generated here and signed by itself. Browsers show a
// warning the first time; that warning is accurate and unavoidable, and it is
// the price of media working at all off localhost.
//
// The certificate covers every address the machine actually answers on, which
// matters more than it sounds: a certificate valid only for "localhost" makes
// the warning unskippable in some browsers rather than merely present.

// certLifetime is deliberately short. A self-signed certificate that lives for
// years is a credential nobody remembers issuing; one that expires in a season
// gets regenerated as a matter of course.
const certLifetime = 90 * 24 * time.Hour

// ensureCert loads a certificate and key, generating a self-signed pair if
// either is missing. It returns the hosts the certificate is valid for.
func ensureCert(certPath, keyPath string) ([]string, error) {
	if fileExists(certPath) && fileExists(keyPath) {
		hosts, err := certHosts(certPath)
		if err != nil {
			return nil, fmt.Errorf("reading existing certificate: %w", err)
		}
		// A laptop moves. The certificate written on yesterday's network does
		// not name today's address, and a browser reaching this server on an
		// address the certificate does not carry does not show the ordinary
		// self-signed warning -- it shows a name mismatch, which on a second
		// device reads as "this server is lying about who it is" and cannot
		// be waved through the same way. Reissue rather than serve it.
		if missing := uncovered(hosts); len(missing) == 0 {
			return hosts, nil
		} else {
			if err := os.Remove(certPath); err != nil {
				return nil, fmt.Errorf("replacing stale certificate: %w", err)
			}
			if err := os.Remove(keyPath); err != nil {
				return nil, fmt.Errorf("replacing stale key: %w", err)
			}
		}
	}

	ips, names := localAddresses()
	return issueCert(certPath, keyPath, ips, names)
}

// issueCert writes a fresh self-signed certificate naming exactly the given
// addresses. Split out from ensureCert so a test can issue one for a network
// this machine is not on, which is the situation the reissue exists for.
func issueCert(certPath, keyPath string, ips []net.IP, names []string) ([]string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generating serial: %w", err)
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"Aether (self-signed)"}},
		// Backdated by an hour so a machine whose clock is slightly ahead of
		// the server does not reject a certificate issued seconds ago.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           ips,
		DNSNames:              names,
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("creating certificate: %w", err)
	}

	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshalling key: %w", err)
	}
	// The private key is written 0600. It is self-signed and short-lived, but
	// it is still a key that authenticates this server to everyone on the
	// network, and a world-readable one on a shared machine is somebody
	// else's man in the middle.
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		return nil, err
	}

	out := make([]string, 0, len(ips)+len(names))
	for _, n := range names {
		out = append(out, n)
	}
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	return out, nil
}

// localAddresses returns every address this machine answers on, so the
// certificate is valid wherever a colleague reaches it from.
func localAddresses() ([]net.IP, []string) {
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	names := []string{"localhost"}

	if host, err := os.Hostname(); err == nil && host != "" {
		names = append(names, host, host+".local")
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return ips, names
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			ips = append(ips, ip)
		}
	}
	return ips, names
}

// LANAddresses returns the routable IPv4 addresses, for printing join URLs.
func LANAddresses() []string {
	ips, _ := localAddresses()
	var out []string
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil && !v4.IsLoopback() {
			out = append(out, v4.String())
		}
	}
	return out
}

// uncovered reports which of this machine's current addresses a certificate
// carrying the given hosts does not name.
func uncovered(hosts []string) []string {
	have := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		have[h] = true
	}
	var missing []string
	for _, ip := range LANAddresses() {
		if !have[ip] {
			missing = append(missing, ip)
		}
	}
	return missing
}

func certHosts(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("%s is not PEM", path)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	out := append([]string(nil), c.DNSNames...)
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	return out, nil
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
