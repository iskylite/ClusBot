package work

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type HTTPDiscoverySource struct{ transport *HTTPBenchmarkProvider }

func NewHTTPDiscoverySource(base, key string, scopes []string, token string) (*HTTPDiscoverySource, error) {
	p, err := NewHTTPBenchmarkProvider(base, key, 1, scopes, token)
	if err != nil {
		return nil, err
	}
	return &HTTPDiscoverySource{transport: p}, nil
}
func (s *HTTPDiscoverySource) AllowsDiscovery(source, scope string) bool {
	return s != nil && source == s.transport.connection && s.transport.tools[scope]
}
func (s *Store) InstallDiscoverySource(ctx context.Context, source *HTTPDiscoverySource) error {
	if source == nil {
		return ErrDiscoveryInput
	}
	p := source.transport
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO discovery_source_versions VALUES(?,?) ON CONFLICT(source_key) DO NOTHING`, p.connection, p.fingerprint); err != nil {
		return err
	}
	var got string
	if err = tx.QueryRow("SELECT fingerprint FROM discovery_source_versions WHERE source_key=?", p.connection).Scan(&got); err != nil {
		return err
	}
	if got != p.fingerprint {
		return fmt.Errorf("%w：发现来源地址或允许范围变化，请用新的来源标识重新授权", ErrConflict)
	}
	return tx.Commit()
}
func (s *HTTPDiscoverySource) ScanDiscovery(ctx context.Context, source, scope string) ([]DiscoveryObservation, error) {
	if !s.AllowsDiscovery(source, scope) {
		return nil, ErrAuthorizationDenied
	}
	payload := toJSON(map[string]string{"source": source, "scope": scope})
	p := s.transport
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/discover", bytes.NewBufferString(payload))
	if err != nil {
		return nil, err
	}
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.credential)
	res, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("发现来源返回 %d", res.StatusCode)
	}
	const maxBytes = 2 << 20
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBytes || !json.Valid(body) {
		return nil, ErrDiscoveryInput
	}
	var result struct {
		Source       string                 `json:"source"`
		Scope        string                 `json:"scope"`
		Observations []DiscoveryObservation `json:"observations"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&result); err != nil {
		return nil, err
	}
	if result.Source != source || result.Scope != scope || result.Observations == nil {
		return nil, ErrDiscoveryInput
	}
	return result.Observations, nil
}
