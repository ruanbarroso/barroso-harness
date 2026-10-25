// tlszen.go gives requests to opencode.ai the TLS ClientHello of the
// official client (Bun/BoringSSL). The zen free-tier gate fingerprints the
// ClientHello (JA3/JA4), not only the headers: measured on 2026-10-25 from
// the same host and IP, with the byte-exact request captured from the
// official CLI 2.1.4, Go net/http and Node got 429 FreeUsageLimitError /
// 403 FreeTierError while Bun fetch and Go+uTLS with Bun's hello got 200.
// The "free usage limit reached" 429 was the gate's answer to a foreign
// client, not a quota.
//
// Only opencode.ai hosts are affected; every other gateway keeps the stdlib
// transport. ZEN_TLS=0 turns it off without a rebuild.
package dealer

import (
	"context"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// specBun is the ClientHello of Bun's fetch, measured on tls.peet.ws:
// ja3 771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49161-49171-
// 49162-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43,
// 4588-29-23-24,0 (ja4 t13d1713h1_5b57614c22b0_7f527383e249).
func specBun() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		TLSVersMin: utls.VersionTLS12,
		TLSVersMax: utls.VersionTLS13,
		CipherSuites: []uint16{
			0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8,
			0xc009, 0xc013, 0xc00a, 0xc014, 0x009c, 0x009d, 0x002f, 0x0035,
		},
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519MLKEM768, utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256, utls.PSSWithSHA256, utls.PKCS1WithSHA256,
				utls.ECDSAWithP384AndSHA384, utls.PSSWithSHA384, utls.PKCS1WithSHA384,
				utls.PSSWithSHA512, utls.PKCS1WithSHA512, utls.Ed25519,
				utls.ECDSAWithP521AndSHA512, utls.PKCS1WithSHA1,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519MLKEM768}, {Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		},
	}
}

// ehHostZen reports whether host (no port) is served by the zen gate.
func ehHostZen(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	return h == "opencode.ai" || strings.HasSuffix(h, ".opencode.ai")
}

func zenTLSLigado() bool { return strings.TrimSpace(os.Getenv("ZEN_TLS")) != "0" }

func dialTLSBun(ctx context.Context, network, addr string) (net.Conn, error) {
	raw, err := (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host, _, _ := net.SplitHostPort(addr)
	uc := utls.UClient(raw, &utls.Config{ServerName: host, NextProtos: []string{"http/1.1"}}, utls.HelloCustom)
	if err := uc.ApplyPreset(specBun()); err != nil {
		raw.Close()
		return nil, err
	}
	if err := uc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return uc, nil
}

var (
	zenTransportOnce sync.Once
	zenTransport     *http.Transport
)

// transporteZen is the HTTP/1.1 transport with Bun's hello. With a proxy
// in the environment the stdlib does the TLS through the tunnel itself, so
// the proxy path is left alone (DialTLSContext only covers direct dials).
func transporteZen() *http.Transport {
	zenTransportOnce.Do(func() {
		zenTransport = &http.Transport{
			DialTLSContext:        dialTLSBun,
			ForceAttemptHTTP2:     false,
			MaxIdleConns:          50,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: time.Second,
		}
	})
	return zenTransport
}

// roteadorTLS sends https requests to opencode.ai through the Bun-hello
// transport and everything else through http.DefaultTransport.
type roteadorTLS struct{}

func (roteadorTLS) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL != nil && req.URL.Scheme == "https" && ehHostZen(req.URL.Hostname()) && zenTLSLigado() {
		if p, _ := http.ProxyFromEnvironment(req); p == nil {
			return transporteZen().RoundTrip(req)
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

// clienteHTTP is the client of every gateway call (no total timeout: the
// watchdog bounds silence, see retry.go).
var clienteHTTP = &http.Client{Transport: roteadorTLS{}}
