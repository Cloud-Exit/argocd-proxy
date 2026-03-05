package server

import (
	"crypto/subtle"
	"sync"

	"github.com/cloud-exit/argocd-cluster-proxy/pkg/tunnel"
)

// Cluster represents a connected remote cluster.
type Cluster struct {
	ID         string
	Token      string          // pre-shared auth token
	TargetAddr string          // address the agent dials locally (e.g. "kubernetes.default.svc:443")
	session    *tunnel.Session
}

// Registry tracks connected clusters.
type Registry struct {
	mu       sync.RWMutex
	clusters map[string]*Cluster // id -> cluster
}

// NewRegistry creates a cluster registry pre-loaded with the given allowed
// clusters. Agents that present a matching token are mapped to the
// corresponding cluster entry.
func NewRegistry(clusters []ClusterConfig) *Registry {
	r := &Registry{
		clusters: make(map[string]*Cluster, len(clusters)),
	}
	for _, cfg := range clusters {
		target := cfg.TargetAddr
		if target == "" {
			target = "kubernetes.default.svc:443"
		}
		r.clusters[cfg.ID] = &Cluster{
			ID:         cfg.ID,
			Token:      cfg.Token,
			TargetAddr: target,
		}
	}
	return r
}

// ClusterConfig defines a cluster that agents may connect as.
type ClusterConfig struct {
	ID         string `json:"id"`
	Token      string `json:"token"`
	TargetAddr string `json:"targetAddr,omitempty"`
}

// Authenticate validates the token using constant-time comparison and returns
// the cluster ID, or empty string if invalid. [C1 fix: timing-safe auth]
func (r *Registry) Authenticate(token string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tokenBytes := []byte(token)
	for _, c := range r.clusters {
		if subtle.ConstantTimeCompare(tokenBytes, []byte(c.Token)) == 1 {
			return c.ID
		}
	}
	return ""
}

// Attach binds a tunnel session to a cluster.
func (r *Registry) Attach(clusterID string, sess *tunnel.Session) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.clusters[clusterID]
	if !ok {
		return false
	}
	// Close existing session if any.
	if c.session != nil {
		c.session.Close()
	}
	c.session = sess
	return true
}

// Get returns the cluster if it has an active session, nil otherwise.
func (r *Registry) Get(id string) *Cluster {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c := r.clusters[id]
	if c == nil || c.session == nil {
		return nil
	}
	return c
}

// Detach removes the session from a cluster.
func (r *Registry) Detach(clusterID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.clusters[clusterID]; ok {
		c.session = nil
	}
}

// Connected returns the IDs of all clusters with active sessions.
func (r *Registry) Connected() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var ids []string
	for id, c := range r.clusters {
		if c.session != nil {
			ids = append(ids, id)
		}
	}
	return ids
}
