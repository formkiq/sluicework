package personal

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Network struct {
	Server string
	HTTP   *http.Client
}

func NewNetwork(server, ca string) (*Network, error) {
	origin, err := Origin(server)
	if err != nil {
		return nil, err
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, err
	}
	if ca != "" && !roots.AppendCertsFromPEM([]byte(ca)) {
		return nil, errors.New("invalid verified cluster CA")
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	return &Network{origin, &http.Client{Transport: t, Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (n *Network) post(ctx context.Context, path string, body, out any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", n.Server+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := n.HTTP.Do(req)
	if err != nil {
		return errors.New("personal agent cannot reach the verified cluster endpoint")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return fmt.Errorf("personal agent synchronization returned HTTP %d", res.StatusCode)
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(out); err != nil {
		return errors.New("invalid personal agent synchronization response")
	}
	return nil
}
func (n *Network) Enroll(ctx context.Context, key ed25519.PrivateKey, ticket, code, name string) (Runtime, error) {
	public := key.Public().(ed25519.PublicKey)
	input := Enrollment{TicketID: ticket, Code: code, Name: name, PublicKey: base64.RawURLEncoding.EncodeToString(public), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, EnrollmentMessage(ticket, public)))}
	var runtime Runtime
	err := n.post(ctx, "/api/v1/personal-agents/enroll", input, &runtime)
	if err == nil && runtime.ID != RuntimeID(public) {
		return Runtime{}, errors.New("cluster returned a different personal runtime identity")
	}
	return runtime, err
}
func (n *Network) Sync(ctx context.Context, key ed25519.PrivateKey, report Report) ([]Proposal, error) {
	payload, _ := json.Marshal(report)
	nonce, err := randomValue()
	if err != nil {
		return nil, err
	}
	e := Envelope{RuntimeID: RuntimeID(key.Public().(ed25519.PublicKey)), Timestamp: time.Now().Unix(), Nonce: nonce, Payload: payload}
	e.Sign(key)
	var proposals []Proposal
	err = n.post(ctx, "/api/v1/personal-agents/sync", e, &proposals)
	return proposals, err
}
